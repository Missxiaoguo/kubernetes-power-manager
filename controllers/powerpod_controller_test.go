package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/go-logr/logr"

	//"k8s.io/apimachinery/pkg/api/errors"
	"go.uber.org/zap/zapcore"
	grpc "google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	powerv1 "github.com/intel/kubernetes-power-manager/api/v1"
	"github.com/intel/kubernetes-power-manager/pkg/podresourcesclient"
	"github.com/intel/kubernetes-power-manager/pkg/podstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	//"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Your mock client probably has a status writer struct like this
type errSubResourceClient struct {
	*errClient
}

func (e *errSubResourceClient) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return fmt.Errorf("mock client Create error")
}

func (e *errSubResourceClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return fmt.Errorf("mock client Update error")
}

func (e *errSubResourceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return fmt.Errorf("mock client Patch error")
}

type fakePodResourcesClient struct {
	listResponse *podresourcesapi.ListPodResourcesResponse
}

func (f *fakePodResourcesClient) List(ctx context.Context, in *podresourcesapi.ListPodResourcesRequest, opts ...grpc.CallOption) (*podresourcesapi.ListPodResourcesResponse, error) {
	return f.listResponse, nil
}

func (f *fakePodResourcesClient) GetAllocatableResources(ctx context.Context, in *podresourcesapi.AllocatableResourcesRequest, opts ...grpc.CallOption) (*podresourcesapi.AllocatableResourcesResponse, error) {
	return &podresourcesapi.AllocatableResourcesResponse{}, nil
}

func (f *fakePodResourcesClient) Get(ctx context.Context, in *podresourcesapi.GetPodResourcesRequest, opts ...grpc.CallOption) (*podresourcesapi.GetPodResourcesResponse, error) {
	return &podresourcesapi.GetPodResourcesResponse{}, nil
}

func createFakePodResourcesListerClient(fakePodResources []*podresourcesapi.PodResources) *podresourcesclient.PodResourcesClient {
	fakeListResponse := &podresourcesapi.ListPodResourcesResponse{
		PodResources: fakePodResources,
	}

	podResourcesListerClient := &fakePodResourcesClient{}
	podResourcesListerClient.listResponse = fakeListResponse
	return &podresourcesclient.PodResourcesClient{Client: podResourcesListerClient, CpuControlPlaneClient: podResourcesListerClient}
}

func createPodReconcilerObject(objs []runtime.Object, podResourcesClient *podresourcesclient.PodResourcesClient) (*PowerPodReconciler, error) {
	log.SetLogger(zap.New(
		zap.UseDevMode(true),
		func(opts *zap.Options) {
			opts.TimeEncoder = zapcore.ISO8601TimeEncoder
		},
	),
	)
	// register operator types with the runtime scheme.
	s := scheme.Scheme

	// add route Openshift scheme
	if err := powerv1.AddToScheme(s); err != nil {
		return nil, err
	}

	// create a fake client to mock API calls.
	cl := fake.NewClientBuilder().
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&powerv1.PowerWorkload{}).
		Build()
	state, err := podstate.NewState()
	if err != nil {
		return nil, err
	}

	// create a ReconcileNode object with the scheme and fake client.
	mockPowerLibrary := new(hostMock)
	// Set up mock to return valid pools for profiles that should work
	mockPowerLibrary.On("GetExclusivePool", "performance").Return(new(poolMock))
	mockPowerLibrary.On("GetExclusivePool", "balance-performance").Return(new(poolMock))
	mockPowerLibrary.On("GetExclusivePool", "universal").Return(new(poolMock))
	mockPowerLibrary.On("GetExclusivePool", "zone-specific").Return(new(poolMock))
	// Return nil for profiles that should fail validation
	mockPowerLibrary.On("GetExclusivePool", "gpu-optimized").Return(nil)
	mockPowerLibrary.On("GetExclusivePool", "nonexistent").Return(nil)

	r := &PowerPodReconciler{
		Client:             cl,
		Log:                ctrl.Log.WithName("testing"),
		Scheme:             s,
		State:              state,
		PodResourcesClient: *podResourcesClient,
		PowerLibrary:       mockPowerLibrary,
	}

	return r, nil
}

var defaultResources = corev1.ResourceRequirements{
	Limits: map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceName("cpu"):                         *resource.NewQuantity(3, resource.DecimalSI),
		corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
		corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
	},
	Requests: map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceName("cpu"):                         *resource.NewQuantity(3, resource.DecimalSI),
		corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
		corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
	},
}

var defaultProfile = &powerv1.PowerProfile{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "performance",
		Namespace: IntelPowerNamespace,
	},
	Spec: powerv1.PowerProfileSpec{
		Name: "performance",
	},
}

var defaultWorkload = &powerv1.PowerWorkload{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "performance-TestNode",
		Namespace: IntelPowerNamespace,
	},
	Spec: powerv1.PowerWorkloadSpec{
		Name:         "performance-TestNode",
		PowerProfile: "performance",
	},
	Status: powerv1.PowerWorkloadStatus{
		WorkloadNodes: powerv1.WorkloadNode{
			Name: "TestNode",
			Containers: []powerv1.Container{
				{PowerProfile: "performance"},
			},
			CpuIds: []uint{},
		},
	},
}

// runs through some basic cases for the controller with no errors
func TestPowerPod_Reconcile_Create(t *testing.T) {
	tcases := []struct {
		testCase        string
		nodeName        string
		podName         string
		podResources    []*podresourcesapi.PodResources
		clientObjs      []runtime.Object
		workloadToCores map[string][]uint
	}{
		{
			testCase: "Test Case 1 - Single container",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-TestNode",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "performance-TestNode",
						PowerProfile: "performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name: "TestNode",
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "test-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadToCores: map[string][]uint{"performance-TestNode": {1, 5, 8}},
		},
		{
			testCase: "Test Case 2 - Two containers",
			nodeName: "TestNode",
			podName:  "test-pod-2",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-2",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
						{
							Name:   "test-container-2",
							CpuIds: []int64{4, 5, 6},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-2",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
							{
								Name:      "test-container-2",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
							{
								Name:        "example-container-2",
								ContainerID: "docker://hijklmnop",
							},
						},
					},
				},
			},
			workloadToCores: map[string][]uint{"performance-TestNode": {1, 2, 3, 4, 5, 6}},
		},
		{
			testCase: "Test Case 3 - More Than One Profile",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
						{
							Name:   "test-container-2",
							CpuIds: []int64{4, 5, 6},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultNode,
				defaultProfile,
				defaultWorkload,
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance-TestNode",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "balance-performance-TestNode",
						PowerProfile: "balance-performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       "TestNode",
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
							{
								Name: "test-container-2",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                                 *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                              *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/balance-performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                                 *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                              *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/balance-performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
								},
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
							{
								Name:        "example-container-2",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadToCores: map[string][]uint{"performance-TestNode": {1, 2, 3}, "balance-performance-TestNode": {4, 5, 6}},
		},
		{
			testCase: "Test Case 4 - Device plugin",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				&powerv1.PowerNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "TestNode",
						Namespace: IntelPowerNamespace,
					},
					Status: powerv1.PowerNodeStatus{
						CustomDevices: []string{"device-plugin"},
					},
				},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-TestNode",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name: "performance-TestNode",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name: "TestNode",
							Containers: []powerv1.Container{
								{
									Name:          "test-container-1",
									ExclusiveCPUs: []uint{1, 5, 8},
								},
							},
							CpuIds: []uint{1, 5, 8},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("device-plugin"):               *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("device-plugin"):               *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
								},
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSBestEffort,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "test-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadToCores: map[string][]uint{"performance-TestNode": {1, 5, 8}},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject(tc.clientObjs, podResourcesClient)
		assert.Nil(t, err)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: IntelPowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.Nil(t, err)
		for test_workload, cores := range tc.workloadToCores {
			workload := &powerv1.PowerWorkload{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      test_workload,
				Namespace: IntelPowerNamespace,
			}, workload)
			assert.Nil(t, err)

			sortedCpuIds := workload.Status.WorkloadNodes.CpuIds
			sort.Slice(workload.Status.WorkloadNodes.CpuIds, func(i, j int) bool {
				return workload.Status.WorkloadNodes.CpuIds[i] < workload.Status.WorkloadNodes.CpuIds[j]
			})
			if !reflect.DeepEqual(cores, sortedCpuIds) {
				t.Errorf("%s failed: expected CPU Ids to be %v, got %v", tc.testCase, cores, sortedCpuIds)
			}
		}
	}
}

// ensures duplicate containers don't get added to workloads
func TestPowerPod_Duplicate_Containers(t *testing.T) {
	nodeName := "TestNode"
	podName := "test-pod-1"
	workloadName := "performance-TestNode"
	podResources := []*podresourcesapi.PodResources{
		{
			Name:      podName,
			Namespace: IntelPowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{
				{
					Name:   "test-container-1",
					CpuIds: []int64{1, 5, 8},
				},
			},
		},
	}
	clientObjs := []runtime.Object{
		defaultNode,
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "TestNode",
			},
		},
		defaultProfile,
		&powerv1.PowerWorkload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workloadName,
				Namespace: IntelPowerNamespace,
			},
			Spec: powerv1.PowerWorkloadSpec{
				Name:         workloadName,
				PowerProfile: "performance",
			},
			Status: powerv1.PowerWorkloadStatus{
				WorkloadNodes: powerv1.WorkloadNode{
					Name: "TestNode",
					Containers: []powerv1.Container{
						{
							Name:          "test-container-1",
							Id:            "abcdefg",
							ExclusiveCPUs: []uint{1, 5, 8},
							PowerProfile:  "performance",
						},
					},
					CpuIds: []uint{1, 5, 8},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: IntelPowerNamespace,
				UID:       "abcdefg",
			},
			Spec: corev1.PodSpec{
				NodeName: "TestNode",
				Containers: []corev1.Container{
					{
						Name:      "test-container-1",
						Resources: defaultResources,
					},
				},
				EphemeralContainers: []corev1.EphemeralContainer{},
			},
			Status: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "test-container-1",
						ContainerID: "docker://abcdefg",
					},
				},
			},
		},
	}
	t.Setenv("NODE_NAME", nodeName)

	podResourcesClient := createFakePodResourcesListerClient(podResources)

	r, err := createPodReconcilerObject(clientObjs, podResourcesClient)
	assert.Nil(t, err)

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      podName,
			Namespace: IntelPowerNamespace,
		},
	}

	_, err = r.Reconcile(context.TODO(), req)
	assert.Nil(t, err)

	workload := &powerv1.PowerWorkload{}
	err = r.Client.Get(context.TODO(), client.ObjectKey{
		Name:      workloadName,
		Namespace: IntelPowerNamespace,
	}, workload)
	assert.Nil(t, err)

	for i, con1 := range workload.Status.WorkloadNodes.Containers {
		for j := i + 1; j < len(workload.Status.WorkloadNodes.Containers); j++ {
			con2 := workload.Status.WorkloadNodes.Containers[j]
			if con1.Id == con2.Id && reflect.DeepEqual(con1.ExclusiveCPUs, con2.ExclusiveCPUs) {
				t.Error("duplicate container not filtered out")
			}
		}
	}
}

// tests where the workload associated with the profile requested does not exist
func TestPowerPod_Reconcile_NonExistingWorkload(t *testing.T) {
	tcases := []struct {
		testCase       string
		nodeName       string
		podName        string
		podResources   []*podresourcesapi.PodResources
		clientObjs     []runtime.Object
		workloadName   string
		expectedCpuIds []uint
	}{
		{
			testCase: "Test Case 1 - Workload does not exist",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "test-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
		},
	}
	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject(tc.clientObjs, podResourcesClient)
		assert.Nil(t, err)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: IntelPowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.ErrorContains(t, err, "recoverable")

		workload := &powerv1.PowerWorkload{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name:      tc.workloadName,
			Namespace: IntelPowerNamespace,
		}, workload)
		assert.ErrorContains(t, err, "not found")
	}
}

// tests for error cases involving invalid pods
func TestPowerPod_Reconcile_ControllerErrors(t *testing.T) {
	tcases := []struct {
		testCase      string
		nodeName      string
		podName       string
		podResources  []*podresourcesapi.PodResources
		clientObjs    []runtime.Object
		workloadNames []string
	}{
		{
			testCase: "Test Case 1 - Pod Not Running error",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodPending,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
		{
			testCase: "Test Case 2 - No Pod UID error",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
		{
			testCase: "Test Case 3 - Resource Mismatch error",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                         *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(2, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                         *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(2, resource.DecimalSI),
									},
								},
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
		{
			testCase: "Test Case 4 - Profile CR Does Not Exist error",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject(tc.clientObjs, podResourcesClient)
		assert.Nil(t, err)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: IntelPowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if err == nil {
			t.Errorf("%s failed: expected the pod controller to have failed", tc.testCase)
		}

		for _, workloadName := range tc.workloadNames {
			workload := &powerv1.PowerWorkload{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      workloadName,
				Namespace: IntelPowerNamespace,
			}, workload)
			assert.Nil(t, err)

			if len(workload.Status.WorkloadNodes.CpuIds) > 0 {
				t.Errorf("%s failed: expected the CPU Ids to be empty, got %v", tc.testCase, workload.Status.WorkloadNodes.CpuIds)
			}
		}
	}
}

func TestPowerPod_Reconcile_ControllerReturningNil(t *testing.T) {
	tcases := []struct {
		testCase      string
		nodeName      string
		podName       string
		namespace     string
		podResources  []*podresourcesapi.PodResources
		clientObjs    []runtime.Object
		workloadNames []string
	}{
		{
			testCase:  "Test Case 1 - Incorrect Node error",
			nodeName:  "TestNode",
			podName:   "test-pod-1",
			namespace: IntelPowerNamespace,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "IncorrectNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
		{
			testCase:  "Test Case 2 - Kube-System Namespace error",
			nodeName:  "TestNode",
			podName:   "test-pod-1",
			namespace: "kube-system",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: "kube-system",
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: "kube-system",
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name:      "test-container-1",
								Resources: defaultResources,
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
		{
			testCase:  "Test Case 3 - Not Exclusive Pod error",
			nodeName:  "TestNode",
			podName:   "test-pod-1",
			namespace: IntelPowerNamespace,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                         *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                      *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                         *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
								},
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSBurstable,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "example-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
			workloadNames: []string{
				"performance-TestNode",
			},
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject(tc.clientObjs, podResourcesClient)
		assert.Nil(t, err)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: tc.namespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.Nil(t, err)

		for _, workloadName := range tc.workloadNames {
			workload := &powerv1.PowerWorkload{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      workloadName,
				Namespace: IntelPowerNamespace,
			}, workload)
			assert.Nil(t, err)

			if len(workload.Status.WorkloadNodes.CpuIds) > 0 {
				t.Errorf("%s failed: expected the CPU Ids to be empty, got %v", tc.testCase, workload.Status.WorkloadNodes.CpuIds)
			}
		}
	}
}

// ensures workloads remove cores upon pod deletion correctly
func TestPowerPod_Reconcile_Delete(t *testing.T) {
	tcases := []struct {
		testCase      string
		nodeName      string
		podName       string
		podResources  []*podresourcesapi.PodResources
		clientObjs    []runtime.Object
		guaranteedPod powerv1.GuaranteedPod
		workloadName  string
	}{
		{
			testCase: "Test Case 1: Single Container",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-TestNode",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "performance-TestNode",
						PowerProfile: "performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name: "TestNode",
							Containers: []powerv1.Container{
								{
									Name:          "existing container",
									ExclusiveCPUs: []uint{1, 2, 3},
									PowerProfile:  "performance",
								},
							},
							CpuIds: []uint{1, 2, 3, 4},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-pod-1",
						Namespace:         IntelPowerNamespace,
						UID:               "abcdefg",
						DeletionTimestamp: &metav1.Time{Time: time.Date(9999, time.Month(1), 21, 1, 10, 30, 0, time.UTC)},
						Finalizers:        []string{"intel.com/finalizer"},
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
					},
				},
			},
			guaranteedPod: powerv1.GuaranteedPod{
				Node:      "TestNode",
				Name:      "test-pod-1",
				Namespace: IntelPowerNamespace,
				UID:       "abcdefg",
				Containers: []powerv1.Container{
					{
						Name:          "test-container-1",
						Id:            "abcdefg",
						Pod:           "test-pod-1",
						ExclusiveCPUs: []uint{1, 2, 3},
						PowerProfile:  "performance",
						Workload:      "performance-TestNode",
					},
				},
			},
			workloadName: "performance-TestNode",
		},
	}

	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)

		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject(tc.clientObjs, podResourcesClient)
		assert.Nil(t, err)

		err = r.State.UpdateStateGuaranteedPods(tc.guaranteedPod)
		assert.Nil(t, err)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: IntelPowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.Nil(t, err)

		workload := &powerv1.PowerWorkload{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name:      tc.workloadName,
			Namespace: IntelPowerNamespace,
		}, workload)
		assert.Nil(t, err)

		if len(workload.Status.WorkloadNodes.CpuIds) != 1 {
			t.Errorf(
				"%s failed: expected one remaining core in the workload, got %v",
				tc.testCase, workload.Status.WorkloadNodes.CpuIds)
		}
	}
}

// uses errclient to mock errors from the client
func TestPowerPod_Reconcile_PodClientErrs(t *testing.T) {
	var deletedPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-pod-1",
			Namespace:         IntelPowerNamespace,
			UID:               "abcdefg",
			DeletionTimestamp: &metav1.Time{Time: time.Date(9999, time.Month(1), 21, 1, 10, 30, 0, time.UTC)},
			Finalizers:        []string{"intel.com/finalizer"},
		},
		Spec: corev1.PodSpec{
			NodeName: "TestNode",
		},
	}
	var defaultPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-1",
			Namespace: IntelPowerNamespace,
			UID:       "abcdefg",
		},
		Spec: corev1.PodSpec{
			NodeName: "TestNode",
			Containers: []corev1.Container{
				{
					Name:      "test-container-1",
					Resources: defaultResources,
				},
			},
			EphemeralContainers: []corev1.EphemeralContainer{},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSGuaranteed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "test-container-1",
					ContainerID: "docker://abcdefg",
				},
			},
		},
	}
	tcases := []struct {
		testCase      string
		nodeName      string
		podName       string
		powerNodeName string
		convertClient func(client.Client) client.Client
		clientErr     string
		podResources  []*podresourcesapi.PodResources
		guaranteedPod powerv1.GuaranteedPod
	}{
		{
			testCase: "Test Case 1 - Invalid Get requests",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			convertClient: func(c client.Client) client.Client {
				mkcl := new(errClient)
				mkcl.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("client get error"))
				return mkcl
			},
			clientErr:    "client get error",
			podResources: []*podresourcesapi.PodResources{},
		},
		{
			testCase: "Test Case 2 - Invalid Update requests",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			convertClient: func(c client.Client) client.Client {
				mkcl := new(errClient)
				mkcl.On("Status").Return(&errSubResourceClient{mkcl})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.Pod")).Return(nil).Run(func(args mock.Arguments) {
					pod := args.Get(2).(*corev1.Pod)
					*pod = *deletedPod
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerWorkload")).Return(nil).Run(func(args mock.Arguments) {
					wload := args.Get(2).(*powerv1.PowerWorkload)
					*wload = *defaultWload
				})
				return mkcl
			},
			clientErr: "client Patch error",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 2, 3},
						},
					},
				},
			},
			guaranteedPod: powerv1.GuaranteedPod{
				Node:      "TestNode",
				Name:      "test-pod-1",
				Namespace: IntelPowerNamespace,
				UID:       "abcdefg",
				Containers: []powerv1.Container{
					{
						Name:          "test-container-1",
						Id:            "abcdefg",
						Pod:           "test-pod-1",
						ExclusiveCPUs: []uint{1, 2, 3},
						PowerProfile:  "performance",
						Workload:      "performance-TestNode",
					},
				},
			},
		},
		{
			testCase: "Test Case 3 - Invalid List requests",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			convertClient: func(c client.Client) client.Client {
				mkcl := new(errClient)
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.Pod")).Return(nil).Run(func(args mock.Arguments) {
					node := args.Get(2).(*corev1.Pod)
					*node = *defaultPod
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerWorkload")).Return(nil).Run(func(args mock.Arguments) {
					wload := args.Get(2).(*powerv1.PowerWorkload)
					*wload = *defaultWload
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerProfile")).Return(fmt.Errorf("powerprofiles.power.intel.com \"performance\" not found"))
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerNode")).Return(nil).Run(func(args mock.Arguments) {
					pnode := args.Get(2).(*powerv1.PowerNode)
					*pnode = *defaultNode
				})
				return mkcl
			},
			clientErr: "",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
		},
		{
			testCase: "Test Case 4 - Invalid node get requests",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			convertClient: func(c client.Client) client.Client {
				mkcl := new(errClient)
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.Pod")).Return(nil).Run(func(args mock.Arguments) {
					node := args.Get(2).(*corev1.Pod)
					*node = *defaultPod
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerWorkload")).Return(nil).Run(func(args mock.Arguments) {
					wload := args.Get(2).(*powerv1.PowerWorkload)
					*wload = *defaultWload
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerProfile")).Return(fmt.Errorf("powerprofiles.power.intel.com \"performance\" not found"))
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerNode")).Return(fmt.Errorf("client  powernode get error"))
				return mkcl
			},
			clientErr: "client  powernode get error",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
		},
	}
	for _, tc := range tcases {
		t.Setenv("NODE_NAME", tc.nodeName)
		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject([]runtime.Object{}, podResourcesClient)
		assert.Nil(t, err)
		err = r.State.UpdateStateGuaranteedPods(tc.guaranteedPod)
		assert.Nil(t, err)
		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: IntelPowerNamespace,
			},
		}
		r.Client = tc.convertClient(r.Client)
		_, err = r.Reconcile(context.TODO(), req)
		if tc.clientErr == "" {
			assert.NoError(t, err)
		} else {
			assert.ErrorContains(t, err, tc.clientErr)
		}

	}

}

func TestPowerPod_ControlPLaneSocket(t *testing.T) {
	tcases := []struct {
		testCase     string
		nodeName     string
		podName      string
		podResources []*podresourcesapi.PodResources
		clientObjs   []runtime.Object
		validateErr  func(t *testing.T, e error)
	}{
		{
			testCase: "Using control plane socket",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			validateErr: func(t *testing.T, err error) {
				assert.Nil(t, err)
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Claims: []corev1.ResourceClaim{{Name: "test-claim"}},
								},
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSBestEffort,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "test-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
		},
		{
			testCase: "Mismatched cores/requests",
			nodeName: "TestNode",
			podName:  "test-pod-1",
			validateErr: func(t *testing.T, err error) {
				assert.ErrorContains(t, err, "recoverable errors")
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container-1",
							CpuIds: []int64{1, 5, 8},
						},
					},
				},
			},
			clientObjs: []runtime.Object{
				defaultNode,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				defaultWload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: IntelPowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(2, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.intel.com/performance"): *resource.NewQuantity(2, resource.DecimalSI),
									},
									Claims: []corev1.ResourceClaim{{Name: "test-claim"}},
								},
							},
						},
						EphemeralContainers: []corev1.EphemeralContainer{},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSBestEffort,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "test-container-1",
								ContainerID: "docker://abcdefg",
							},
						},
					},
				},
			},
		},
	}
	for i, tc := range tcases {
		t.Logf("Test Case %d: %s", i+1, tc.testCase)
		t.Setenv("NODE_NAME", tc.nodeName)

		podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

		r, err := createPodReconcilerObject(tc.clientObjs, podResourcesClient)
		assert.Nil(t, err)

		req := reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      tc.podName,
				Namespace: IntelPowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		tc.validateErr(t, err)

	}
}

// tests positive and negative cases for SetupWithManager function
func TestPowerPod_Reconcile_SetupPass(t *testing.T) {
	podResources := []*podresourcesapi.PodResources{}
	podResourcesClient := createFakePodResourcesListerClient(podResources)
	r, err := createPodReconcilerObject([]runtime.Object{}, podResourcesClient)
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("SetFields", mock.Anything).Return(nil)
	mgr.On("Add", mock.Anything).Return(nil)
	mgr.On("GetCache").Return(new(cacheMk))
	err = (&PowerPodReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Nil(t, err)

}

func TestPowerPod_Reconcile_SetupFail(t *testing.T) {
	podResources := []*podresourcesapi.PodResources{}
	podResourcesClient := createFakePodResourcesListerClient(podResources)
	r, err := createPodReconcilerObject([]runtime.Object{}, podResourcesClient)
	assert.Nil(t, err)
	mgr := new(mgrMock)
	mgr.On("GetControllerOptions").Return(config.Controller{})
	mgr.On("GetScheme").Return(r.Scheme)
	mgr.On("GetLogger").Return(r.Log)
	mgr.On("Add", mock.Anything).Return(fmt.Errorf("setup fail"))
	err = (&PowerPodReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}).SetupWithManager(mgr)
	assert.Error(t, err)

}

func TestPowerPod_getNewWorkloadContainerList(t *testing.T) {
	log := logr.Discard()

	commonContainer := powerv1.Container{
		Name: "common",
		Id:   "common",
	}
	nodeOnlyContainer := powerv1.Container{
		Name: "node-only",
		Id:   "node-only",
	}
	podStateOnlyContainer := powerv1.Container{
		Name: "podstate-only",
		Id:   "podstate-only",
	}

	nodeContainers := []powerv1.Container{nodeOnlyContainer, commonContainer}
	podstateContainers := []powerv1.Container{podStateOnlyContainer, commonContainer}

	newNodeContainers := getNewWorkloadContainerList(nodeContainers, podstateContainers, &log)

	assert.ElementsMatch(t, newNodeContainers, []powerv1.Container{nodeOnlyContainer})
}

func TestPowerPodisContainerInList(t *testing.T) {
	logger := logr.Discard()

	// positive test
	containers := []powerv1.Container{
		{
			Name: "test1",
			Id:   "12-34",
		},
	}
	assert.True(t, isContainerInList("test1", "12-34", containers, &logger))

	// negative test
	assert.False(t, isContainerInList("not-in-list", "not-in-list", containers, &logger))
}

func TestPowerPod_ValidateProfileNodeSelectorMatching(t *testing.T) {
	testNode := "TestNode"

	baseNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNode,
			Labels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
				"env":       "production",
			},
		},
	}

	basePowerNode := &powerv1.PowerNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNode,
			Namespace: IntelPowerNamespace,
		},
		Status: powerv1.PowerNodeStatus{
			CustomDevices: []string{},
		},
	}

	matchingProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "performance",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "performance",
			NodeSelector: powerv1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "worker",
					},
				},
			},
		},
	}

	nonMatchingProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-optimized",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "gpu-optimized",
			NodeSelector: powerv1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"node-type": "gpu-node",
					},
				},
			},
		},
	}

	universalProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "universal",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "universal",
			// No node selector - should apply to all nodes
		},
	}

	expressionMatchingProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "zone-specific",
			Namespace: IntelPowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "zone-specific",
			NodeSelector: powerv1.NodeSelector{
				LabelSelector: metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "zone",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"us-west-1a", "us-west-1b"},
						},
						{
							Key:      "env",
							Operator: metav1.LabelSelectorOpExists,
						},
					},
				},
			},
		},
	}

	testCases := []struct {
		name              string
		nodeLabels        map[string]string
		podSpec           corev1.PodSpec
		podStatus         corev1.PodStatus
		profiles          []runtime.Object
		workloads         []runtime.Object
		podResources      []*podresourcesapi.PodResources
		expectError       bool
		expectRecoverable bool
		errorContains     string
		expectedWorkloads map[string][]uint
	}{
		{
			name: "Single profile with matching node selector",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "test-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "test-container",
						ContainerID: "docker://abc123",
					},
				},
			},
			profiles: []runtime.Object{matchingProfile},
			workloads: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "performance-" + testNode,
						PowerProfile: "performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{1, 2},
						},
					},
				},
			},
			expectError:       false,
			expectedWorkloads: map[string][]uint{"performance-" + testNode: {1, 2}},
		},
		{
			name: "Profile with non-matching node selector",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "test-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                           *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/gpu-optimized": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                           *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/gpu-optimized": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "test-container",
						ContainerID: "docker://abc123",
					},
				},
			},
			profiles: []runtime.Object{nonMatchingProfile},
			workloads: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gpu-optimized-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "gpu-optimized-" + testNode,
						PowerProfile: "gpu-optimized",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{1, 2},
						},
					},
				},
			},
			expectError:       true,
			expectRecoverable: true,
			errorContains: fmt.Sprintf(
				"recoverable errors encountered: power profile '%s' is not available on node %s",
				nonMatchingProfile.Name,
				testNode,
			),
		},
		{
			name: "Universal profile with no node selector",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "test-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                       *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/universal": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                       *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/universal": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "test-container",
						ContainerID: "docker://abc123",
					},
				},
			},
			profiles: []runtime.Object{universalProfile},
			workloads: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "universal-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "universal-" + testNode,
						PowerProfile: "universal",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{3, 4},
						},
					},
				},
			},
			expectError:       false,
			expectedWorkloads: map[string][]uint{"universal-" + testNode: {3, 4}},
		},
		{
			name: "Profile with MatchExpressions selector",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
				"env":       "production",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "test-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                           *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/zone-specific": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                           *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/zone-specific": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "test-container",
						ContainerID: "docker://abc123",
					},
				},
			},
			profiles: []runtime.Object{expressionMatchingProfile},
			workloads: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "zone-specific-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "zone-specific-" + testNode,
						PowerProfile: "zone-specific",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{5, 6},
						},
					},
				},
			},
			expectError:       false,
			expectedWorkloads: map[string][]uint{"zone-specific-" + testNode: {5, 6}},
		},
		{
			name: "Multiple containers with different profiles",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
				"env":       "production",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "performance-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
					{
						Name: "universal-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                       *resource.NewQuantity(1, resource.DecimalSI),
								"power.intel.com/universal": *resource.NewQuantity(1, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                       *resource.NewQuantity(1, resource.DecimalSI),
								"power.intel.com/universal": *resource.NewQuantity(1, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "performance-container",
						ContainerID: "docker://abc123",
					},
					{
						Name:        "universal-container",
						ContainerID: "docker://def456",
					},
				},
			},
			profiles: []runtime.Object{matchingProfile, universalProfile},
			workloads: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "performance-" + testNode,
						PowerProfile: "performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "universal-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "universal-" + testNode,
						PowerProfile: "universal",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "performance-container",
							CpuIds: []int64{7, 8},
						},
						{
							Name:   "universal-container",
							CpuIds: []int64{9},
						},
					},
				},
			},
			expectError: false,
			expectedWorkloads: map[string][]uint{
				"performance-" + testNode: {7, 8},
				"universal-" + testNode:   {9},
			},
		},
		{
			name: "Profile doesn't exist in cluster",
			nodeLabels: map[string]string{
				"node-type": "worker",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "test-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/nonexistent": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/nonexistent": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "test-container",
						ContainerID: "docker://abc123",
					},
				},
			},
			profiles: []runtime.Object{
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "nonexistent",
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "nonexistent",
						NodeSelector: powerv1.NodeSelector{
							LabelSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"node-type": "worker",
								},
							},
						},
					},
				},
			},
			workloads: []runtime.Object{},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{1, 2},
						},
					},
				},
			},
			expectError:       true,
			expectRecoverable: true,
			errorContains: fmt.Sprintf(
				"recoverable errors encountered: power profile 'nonexistent' is not available on node %s",
				testNode,
			),
		},
		{
			name: "Mixed scenario - one matching, one non-matching profile",
			nodeLabels: map[string]string{
				"node-type": "worker",
				"zone":      "us-west-1a",
			},
			podSpec: corev1.PodSpec{
				NodeName: testNode,
				Containers: []corev1.Container{
					{
						Name: "good-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                         *resource.NewQuantity(2, resource.DecimalSI),
								"power.intel.com/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
					{
						Name: "bad-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                           *resource.NewQuantity(1, resource.DecimalSI),
								"power.intel.com/gpu-optimized": *resource.NewQuantity(1, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                           *resource.NewQuantity(1, resource.DecimalSI),
								"power.intel.com/gpu-optimized": *resource.NewQuantity(1, resource.DecimalSI),
							},
						},
					},
				},
			},
			podStatus: corev1.PodStatus{
				Phase:    corev1.PodRunning,
				QOSClass: corev1.PodQOSGuaranteed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "good-container",
						ContainerID: "docker://abc123",
					},
					{
						Name:        "bad-container",
						ContainerID: "docker://def456",
					},
				},
			},
			profiles: []runtime.Object{matchingProfile, nonMatchingProfile},
			workloads: []runtime.Object{
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "performance-" + testNode,
						PowerProfile: "performance",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gpu-optimized-" + testNode,
						Namespace: IntelPowerNamespace,
					},
					Spec: powerv1.PowerWorkloadSpec{
						Name:         "gpu-optimized-" + testNode,
						PowerProfile: "gpu-optimized",
					},
					Status: powerv1.PowerWorkloadStatus{
						WorkloadNodes: powerv1.WorkloadNode{
							Name:       testNode,
							Containers: []powerv1.Container{},
							CpuIds:     []uint{},
						},
					},
				},
			},
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "good-container",
							CpuIds: []int64{10, 11},
						},
						{
							Name:   "bad-container",
							CpuIds: []int64{12},
						},
					},
				},
			},
			expectError:       true,
			expectRecoverable: true,
			errorContains: fmt.Sprintf(
				"recoverable errors encountered: power profile 'gpu-optimized' is not available on node %s",
				testNode,
			),
			expectedWorkloads: map[string][]uint{
				"performance-" + testNode: {10, 11},
				// gpu-optimized workload should not get updated due to selector mismatch
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NODE_NAME", testNode)

			// Create node with specified labels
			node := baseNode.DeepCopy()
			node.Labels = tc.nodeLabels

			// Create PowerNode
			powerNode := basePowerNode.DeepCopy()

			// Create pod with test spec
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
					UID:       "test-uid-123",
				},
				Spec:   tc.podSpec,
				Status: tc.podStatus,
			}

			clientObjs := []runtime.Object{node, powerNode, pod}
			clientObjs = append(clientObjs, tc.profiles...)
			clientObjs = append(clientObjs, tc.workloads...)

			podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

			r, err := createPodReconcilerObject(clientObjs, podResourcesClient)
			assert.NoError(t, err)

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      "test-pod",
					Namespace: IntelPowerNamespace,
				},
			}

			result, err := r.Reconcile(context.TODO(), req)

			if tc.expectError {
				assert.Error(t, err)
				if tc.errorContains != "" {
					assert.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}

			// Check workload updates
			for workloadName, expectedCPUs := range tc.expectedWorkloads {
				workload := &powerv1.PowerWorkload{}
				err = r.Client.Get(context.TODO(), client.ObjectKey{
					Name:      workloadName,
					Namespace: IntelPowerNamespace,
				}, workload)
				assert.NoError(t, err)

				actualCPUs := workload.Status.WorkloadNodes.CpuIds
				sort.Slice(actualCPUs, func(i, j int) bool {
					return actualCPUs[i] < actualCPUs[j]
				})
				sort.Slice(expectedCPUs, func(i, j int) bool {
					return expectedCPUs[i] < expectedCPUs[j]
				})

				assert.ElementsMatch(t, expectedCPUs, actualCPUs,
					"Workload %s should have CPUs %v but got %v", workloadName, expectedCPUs, actualCPUs)
			}

			// Verify result properties
			assert.False(t, result.Requeue)
			assert.Equal(t, time.Duration(0), result.RequeueAfter)
		})
	}
}

func TestPowerReleventPodPredicate(t *testing.T) {
	t.Setenv("NODE_NAME", "TestNode")

	makePod := func(ns, node string, initReqs,
		reqs map[corev1.ResourceName]resource.Quantity,
		withInit bool,
	) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: ns,
			},
			Spec: corev1.PodSpec{
				NodeName: node,
				Containers: []corev1.Container{
					{
						Name: "c1",
						Resources: corev1.ResourceRequirements{
							Requests: reqs,
							Limits:   reqs,
						},
					},
				},
			},
		}
		if withInit {
			pod.Spec.InitContainers = []corev1.Container{
				{
					Name: "ic1",
					Resources: corev1.ResourceRequirements{
						Requests: initReqs,
						Limits:   initReqs,
					},
				},
			}
		}
		return pod
	}

	powerReq := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceName(ResourcePrefix + "performance"): *resource.NewQuantity(1, resource.DecimalSI),
	}
	cpuMemReq := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceCPU:    *resource.NewQuantity(1, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
	}

	cases := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{
			name: "non-pod object returns false",
			obj:  &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}},
			want: false,
		},
		{
			name: "node mismatch returns false",
			obj:  makePod(IntelPowerNamespace, "OtherNode", nil, powerReq, false),
			want: false,
		},
		{
			name: "no power requests returns false",
			obj:  makePod(IntelPowerNamespace, "TestNode", nil, cpuMemReq, false),
			want: false,
		},
		{
			name: "container with power request returns true",
			obj:  makePod(IntelPowerNamespace, "TestNode", nil, powerReq, false),
			want: true,
		},
		{
			name: "container with power request in other namespace returns true",
			obj:  makePod("test-namespace", "TestNode", nil, powerReq, true),
			want: true,
		},
		{
			name: "init container with power request returns true",
			obj:  makePod(IntelPowerNamespace, "TestNode", powerReq, cpuMemReq, true),
			want: true,
		},
		{
			name: "multiple containers where one requests power returns true",
			obj: func() client.Object {
				pod := makePod(IntelPowerNamespace, "TestNode", nil, cpuMemReq, false)
				pod.Spec.Containers = append(
					pod.Spec.Containers,
					corev1.Container{
						Name: "c2",
						Resources: corev1.ResourceRequirements{
							Requests: powerReq,
							Limits:   powerReq,
						},
					},
				)
				return pod
			}(),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PowerReleventPodPredicate(tc.obj)
			if got != tc.want {
				t.Fatalf("PowerReleventPodPredicate() = %v, want %v", got, tc.want)
			}
		})
	}
}
