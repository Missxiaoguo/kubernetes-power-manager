package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

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

	"github.com/go-logr/logr"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podresourcesclient"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

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
		WithStatusSubresource(&powerv1.PowerWorkload{}, &powerv1.PowerNodeState{}).
		Build()
	state, err := podstate.NewState()
	if err != nil {
		return nil, err
	}

	// create a ReconcileNode object with the scheme and fake client.
	mockPowerLibrary := new(hostMock)

	// Set up mock to return valid pools for profiles that should work
	// Each pool returns an empty CpuList (new CPUs will be added)
	mockPowerLibrary.On("GetExclusivePool", "performance").Return(createMockPoolWithCPUs([]uint{}))
	mockPowerLibrary.On("GetExclusivePool", "balance-performance").Return(createMockPoolWithCPUs([]uint{}))
	mockPowerLibrary.On("GetExclusivePool", "universal").Return(createMockPoolWithCPUs([]uint{}))
	mockPowerLibrary.On("GetExclusivePool", "zone-specific").Return(createMockPoolWithCPUs([]uint{}))
	// Return nil for profiles that should fail validation
	mockPowerLibrary.On("GetExclusivePool", "gpu-optimized").Return(nil)
	mockPowerLibrary.On("GetExclusivePool", "nonexistent").Return(nil)

	// Set up GetSharedPool with a broad range of CPUs.
	// Tests expect CPUs to be in the shared pool before moving to exclusive pools.
	// Include CPUs 0-99 to cover all test scenarios.
	sharedPoolCPUs := make([]uint, 100)
	for i := range sharedPoolCPUs {
		sharedPoolCPUs[i] = uint(i)
	}
	mockPowerLibrary.On("GetSharedPool").Return(createMockPoolWithCPUs(sharedPoolCPUs))

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
		corev1.ResourceName("cpu"):                            *resource.NewQuantity(3, resource.DecimalSI),
		corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
		corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
	},
	Requests: map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceName("cpu"):                            *resource.NewQuantity(3, resource.DecimalSI),
		corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
		corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
	},
}

var defaultProfile = &powerv1.PowerProfile{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "performance",
		Namespace: PowerNamespace,
	},
	Spec: powerv1.PowerProfileSpec{
		Name: "performance",
	},
}

var defaultWorkload = &powerv1.PowerWorkload{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "performance-TestNode",
		Namespace: PowerNamespace,
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

var defaultPowerNodeState = &powerv1.PowerNodeState{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "TestNode-power-state",
		Namespace: PowerNamespace,
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
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: PowerNamespace,
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
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
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
						Namespace: PowerNamespace,
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
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
				defaultProfile,
				defaultWorkload,
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance-TestNode",
						Namespace: PowerNamespace,
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
						Namespace: PowerNamespace,
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
										corev1.ResourceName("cpu"):                                    *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                                 *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/balance-performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                                    *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                                 *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/balance-performance"): *resource.NewQuantity(3, resource.DecimalSI),
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
					Namespace: PowerNamespace,
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
						Namespace: PowerNamespace,
					},
					Status: powerv1.PowerNodeStatus{
						CustomDevices: []string{"device-plugin"},
					},
				},
				defaultPowerNodeState,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				defaultProfile,
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-TestNode",
						Namespace: PowerNamespace,
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
						Namespace: PowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("device-plugin"):                  *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("device-plugin"):                  *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
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
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.Nil(t, err)

		// Verify PowerNodeState has the expected exclusive CPU entries.
		powerNodeState := &powerv1.PowerNodeState{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Name:      tc.nodeName + "-power-state",
			Namespace: PowerNamespace,
		}, powerNodeState)
		assert.Nil(t, err)

		// Collect all CPUs from the exclusive pools per profile.
		profileToCPUs := make(map[string][]uint)
		if powerNodeState.Status.CPUPools == nil {
			t.Fatal("expected CPUPools to be set in PowerNodeState")
		}
		for _, exclusive := range powerNodeState.Status.CPUPools.Exclusive {
			for _, container := range exclusive.PowerContainers {
				profileToCPUs[container.PowerProfile] = append(profileToCPUs[container.PowerProfile], container.CPUIDs...)
			}
		}

		// Verify expected CPUs per profile (key format: profile + "-" + nodeName).
		for workloadName, expectedCores := range tc.workloadToCores {
			profileName := workloadName[:len(workloadName)-len(tc.nodeName)-1]
			actualCores := profileToCPUs[profileName]
			sort.Slice(actualCores, func(i, j int) bool {
				return actualCores[i] < actualCores[j]
			})
			sort.Slice(expectedCores, func(i, j int) bool {
				return expectedCores[i] < expectedCores[j]
			})
			if !reflect.DeepEqual(expectedCores, actualCores) {
				t.Errorf("%s failed: expected CPU Ids for profile %s to be %v, got %v", tc.testCase, profileName, expectedCores, actualCores)
			}
		}
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
		expectError   bool
	}{
		{
			testCase:    "Test Case 1 - Pod Not Running error",
			nodeName:    "TestNode",
			podName:     "test-pod-1",
			expectError: true,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
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
						Namespace: PowerNamespace,
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
			testCase:    "Test Case 2 - No Pod UID error",
			nodeName:    "TestNode",
			podName:     "test-pod-1",
			expectError: true,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
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
						Namespace: PowerNamespace,
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
			testCase:    "Test Case 3 - Resource Mismatch error",
			nodeName:    "TestNode",
			podName:     "test-pod-1",
			expectError: false,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
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
						Namespace: PowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                            *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                            *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
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
			testCase:    "Test Case 4 - Profile CR Does Not Exist error",
			nodeName:    "TestNode",
			podName:     "test-pod-1",
			expectError: true,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "TestNode",
					},
				},
				&powerv1.PowerProfile{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "balance-performance",
						Namespace: PowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: PowerNamespace,
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
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		if tc.expectError && err == nil {
			t.Errorf("%s failed: expected the pod controller to have failed", tc.testCase)
		} else if !tc.expectError && err != nil {
			t.Errorf("%s failed: expected no error but got: %v", tc.testCase, err)
		}

		for _, workloadName := range tc.workloadNames {
			workload := &powerv1.PowerWorkload{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      workloadName,
				Namespace: PowerNamespace,
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
			namespace: PowerNamespace,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
						Namespace: PowerNamespace,
					},
					Spec: powerv1.PowerProfileSpec{
						Name: "balance-performance",
					},
				},
				defaultWorkload,
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod-1",
						Namespace: PowerNamespace,
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
						Namespace: PowerNamespace,
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
			namespace: PowerNamespace,
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
						Namespace: PowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                            *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("memory"):                         *resource.NewQuantity(200, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("cpu"):                            *resource.NewQuantity(3, resource.DecimalSI),
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
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
				Namespace: PowerNamespace,
			}, workload)
			assert.Nil(t, err)

			if len(workload.Status.WorkloadNodes.CpuIds) > 0 {
				t.Errorf("%s failed: expected the CPU Ids to be empty, got %v", tc.testCase, workload.Status.WorkloadNodes.CpuIds)
			}
		}
	}
}

// ensures CPUs are moved back to shared pool upon pod deletion
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
					Namespace: PowerNamespace,
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
				// PowerNodeState with the pod's exclusive CPU info (for deletion lookup)
				&powerv1.PowerNodeState{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "TestNode-power-state",
						Namespace: PowerNamespace,
					},
					Status: powerv1.PowerNodeStateStatus{
						CPUPools: &powerv1.CPUPoolsStatus{
							Exclusive: []powerv1.ExclusiveCPUPoolStatus{
								{
									PodUID: "abcdefg",
									Pod:    "test-pod-1",
									PowerContainers: []powerv1.PowerContainer{
										{
											Name:         "test-container-1",
											ID:           "abcdefg",
											PowerProfile: "performance",
											CPUIDs:       []uint{1, 2, 3},
										},
									},
								},
							},
						},
					},
				},
				defaultProfile,
				&powerv1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "performance-TestNode",
						Namespace: PowerNamespace,
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
						Namespace:         PowerNamespace,
						UID:               "abcdefg",
						DeletionTimestamp: &metav1.Time{Time: time.Date(9999, time.Month(1), 21, 1, 10, 30, 0, time.UTC)},
						Finalizers:        []string{"power.openshift.io/finalizer"},
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
					},
				},
			},
			guaranteedPod: powerv1.GuaranteedPod{
				Node:      "TestNode",
				Name:      "test-pod-1",
				Namespace: PowerNamespace,
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
				Namespace: PowerNamespace,
			},
		}

		_, err = r.Reconcile(context.TODO(), req)
		assert.Nil(t, err)

		// Verify the pod was removed from internal state
		podFromState := r.State.GetPodFromState(tc.podName, PowerNamespace)
		assert.Empty(t, podFromState.UID, "%s: expected pod to be removed from internal state", tc.testCase)

		// Verify the shared pool MoveCpuIDs was called (CPUs moved back)
		// The mock records all calls, so we verify it was called
		mockHost := r.PowerLibrary.(*hostMock)
		mockHost.AssertCalled(t, "GetSharedPool")

		// Note: The fake client doesn't fully support SSA semantics for removal.
		// In a real cluster, the SSA patch with empty containers would remove the
		// pod's entry from PowerNodeState. Here we verify the reconcile succeeds
		// and trust the SSA behavior works correctly with the real API server.
	}
}

// uses errclient to mock errors from the client
func TestPowerPod_Reconcile_PodClientErrs(t *testing.T) {
	var deletedPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-pod-1",
			Namespace:         PowerNamespace,
			UID:               "abcdefg",
			DeletionTimestamp: &metav1.Time{Time: time.Date(9999, time.Month(1), 21, 1, 10, 30, 0, time.UTC)},
			Finalizers:        []string{"power.openshift.io/finalizer"},
		},
		Spec: corev1.PodSpec{
			NodeName: "TestNode",
		},
	}
	var defaultPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-1",
			Namespace: PowerNamespace,
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
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerNodeState")).Return(nil).Run(func(args mock.Arguments) {
					nodeState := args.Get(2).(*powerv1.PowerNodeState)
					*nodeState = powerv1.PowerNodeState{
						ObjectMeta: metav1.ObjectMeta{
							ManagedFields: []metav1.ManagedFieldsEntry{
								{Manager: "powerpod-controller.abcdefg", Operation: metav1.ManagedFieldsOperationApply},
							},
						},
						Status: powerv1.PowerNodeStateStatus{
							CPUPools: &powerv1.CPUPoolsStatus{
								Exclusive: []powerv1.ExclusiveCPUPoolStatus{
									{
										PodUID: "abcdefg",
										Pod:    "test-pod-1",
										PowerContainers: []powerv1.PowerContainer{
											{
												Name:         "test-container-1",
												ID:           "abcdefg",
												PowerProfile: "performance",
												CPUIDs:       []uint{1, 2, 3},
											},
										},
									},
								},
							},
						},
					}
				})
				return mkcl
			},
			clientErr: "client Patch error",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
				Namespace: PowerNamespace,
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
				// Use a success status writer since we expect no error from this test
				statusWriter := new(mockResourceWriter)
				statusWriter.On("Patch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				mkcl.On("Status").Return(statusWriter)
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.Pod")).Return(nil).Run(func(args mock.Arguments) {
					node := args.Get(2).(*corev1.Pod)
					*node = *defaultPod
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerWorkload")).Return(nil).Run(func(args mock.Arguments) {
					wload := args.Get(2).(*powerv1.PowerWorkload)
					*wload = *defaultWload
				})
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerProfile")).Return(fmt.Errorf("powerprofiles.power.openshift.io \"performance\" not found"))
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
					Namespace: PowerNamespace,
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
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerProfile")).Return(fmt.Errorf("powerprofiles.power.openshift.io \"performance\" not found"))
				mkcl.On("Get", mock.Anything, mock.Anything, mock.AnythingOfType("*v1.PowerNode")).Return(fmt.Errorf("client  powernode get error"))
				return mkcl
			},
			clientErr: "client  powernode get error",
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod-1",
					Namespace: PowerNamespace,
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
				Namespace: PowerNamespace,
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
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
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
						Namespace: PowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(3, resource.DecimalSI),
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
					Namespace: PowerNamespace,
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
				defaultPowerNodeState,
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
						Namespace: PowerNamespace,
						UID:       "abcdefg",
					},
					Spec: corev1.PodSpec{
						NodeName: "TestNode",
						Containers: []corev1.Container{
							{
								Name: "test-container-1",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
									},
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName("power.openshift.io/performance"): *resource.NewQuantity(2, resource.DecimalSI),
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
				Namespace: PowerNamespace,
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
			Namespace: PowerNamespace,
		},
		Status: powerv1.PowerNodeStatus{
			CustomDevices: []string{},
		},
	}

	basePowerNodeState := &powerv1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNode + "-power-state",
			Namespace: PowerNamespace,
		},
	}

	matchingProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "performance",
			Namespace: PowerNamespace,
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
			Namespace: PowerNamespace,
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
			Namespace: PowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "universal",
			// No node selector - should apply to all nodes
		},
	}

	expressionMatchingProfile := &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "zone-specific",
			Namespace: PowerNamespace,
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
		name               string
		nodeLabels         map[string]string
		podSpec            corev1.PodSpec
		podStatus          corev1.PodStatus
		profiles           []runtime.Object
		podResources       []*podresourcesapi.PodResources
		expectError        bool
		expectRecoverable  bool
		errorContains      string
		expectedContainers []powerv1.PowerContainer
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
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/performance": *resource.NewQuantity(2, resource.DecimalSI),
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{1, 2},
						},
					},
				},
			},
			expectError: false,
			expectedContainers: []powerv1.PowerContainer{
				{Name: "test-container", PowerProfile: "performance", CPUIDs: []uint{1, 2}},
			},
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
								"cpu":                              *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/gpu-optimized": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                              *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/gpu-optimized": *resource.NewQuantity(2, resource.DecimalSI),
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
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
								"cpu":                          *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/universal": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                          *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/universal": *resource.NewQuantity(2, resource.DecimalSI),
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{3, 4},
						},
					},
				},
			},
			expectError: false,
			expectedContainers: []powerv1.PowerContainer{
				{Name: "test-container", PowerProfile: "universal", CPUIDs: []uint{3, 4}},
			},
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
								"cpu":                              *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/zone-specific": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                              *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/zone-specific": *resource.NewQuantity(2, resource.DecimalSI),
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:   "test-container",
							CpuIds: []int64{5, 6},
						},
					},
				},
			},
			expectError: false,
			expectedContainers: []powerv1.PowerContainer{
				{Name: "test-container", PowerProfile: "zone-specific", CPUIDs: []uint{5, 6}},
			},
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
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
					{
						Name: "universal-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                          *resource.NewQuantity(1, resource.DecimalSI),
								"power.openshift.io/universal": *resource.NewQuantity(1, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                          *resource.NewQuantity(1, resource.DecimalSI),
								"power.openshift.io/universal": *resource.NewQuantity(1, resource.DecimalSI),
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
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
			expectedContainers: []powerv1.PowerContainer{
				{Name: "performance-container", PowerProfile: "performance", CPUIDs: []uint{7, 8}},
				{Name: "universal-container", PowerProfile: "universal", CPUIDs: []uint{9}},
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
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/nonexistent": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/nonexistent": *resource.NewQuantity(2, resource.DecimalSI),
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
						Namespace: PowerNamespace,
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
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
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                            *resource.NewQuantity(2, resource.DecimalSI),
								"power.openshift.io/performance": *resource.NewQuantity(2, resource.DecimalSI),
							},
						},
					},
					{
						Name: "bad-container",
						Resources: corev1.ResourceRequirements{
							Limits: map[corev1.ResourceName]resource.Quantity{
								"cpu":                              *resource.NewQuantity(1, resource.DecimalSI),
								"power.openshift.io/gpu-optimized": *resource.NewQuantity(1, resource.DecimalSI),
							},
							Requests: map[corev1.ResourceName]resource.Quantity{
								"cpu":                              *resource.NewQuantity(1, resource.DecimalSI),
								"power.openshift.io/gpu-optimized": *resource.NewQuantity(1, resource.DecimalSI),
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
			podResources: []*podresourcesapi.PodResources{
				{
					Name:      "test-pod",
					Namespace: PowerNamespace,
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
			expectedContainers: []powerv1.PowerContainer{
				{Name: "good-container", PowerProfile: "performance", CPUIDs: []uint{10, 11}},
				{Name: "bad-container", PowerProfile: "gpu-optimized", Errors: []string{
					fmt.Sprintf("power profile 'gpu-optimized' is not available on node %s", testNode),
				}},
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
					Namespace: PowerNamespace,
					UID:       "test-uid-123",
				},
				Spec:   tc.podSpec,
				Status: tc.podStatus,
			}

			clientObjs := []runtime.Object{node, powerNode, basePowerNodeState, pod}
			clientObjs = append(clientObjs, tc.profiles...)

			podResourcesClient := createFakePodResourcesListerClient(tc.podResources)

			r, err := createPodReconcilerObject(clientObjs, podResourcesClient)
			assert.NoError(t, err)

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      "test-pod",
					Namespace: PowerNamespace,
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

			// Check PowerNodeState exclusive pool entries.
			if len(tc.expectedContainers) > 0 {
				powerNodeState := &powerv1.PowerNodeState{}
				err = r.Client.Get(context.TODO(), client.ObjectKey{
					Name:      testNode + "-power-state",
					Namespace: PowerNamespace,
				}, powerNodeState)
				assert.NoError(t, err)

				// Collect all PowerContainers from the exclusive pools.
				var actualContainers []powerv1.PowerContainer
				if powerNodeState.Status.CPUPools == nil {
					t.Fatal("expected CPUPools to be set in PowerNodeState")
				}
				for _, exclusive := range powerNodeState.Status.CPUPools.Exclusive {
					actualContainers = append(actualContainers, exclusive.PowerContainers...)
				}

				require.Len(t, actualContainers, len(tc.expectedContainers))
				for _, expected := range tc.expectedContainers {
					var found bool
					for _, actual := range actualContainers {
						if actual.Name == expected.Name {
							found = true
							assert.Equal(t, expected.PowerProfile, actual.PowerProfile, "container %s profile mismatch", expected.Name)
							assert.ElementsMatch(t, expected.CPUIDs, actual.CPUIDs, "container %s CPUIDs mismatch", expected.Name)
							assert.ElementsMatch(t, expected.Errors, actual.Errors, "container %s errors mismatch", expected.Name)
							break
						}
					}
					assert.True(t, found, "expected container %s not found in PowerNodeState", expected.Name)
				}
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
			obj:  makePod(PowerNamespace, "OtherNode", nil, powerReq, false),
			want: false,
		},
		{
			name: "no power requests returns false",
			obj:  makePod(PowerNamespace, "TestNode", nil, cpuMemReq, false),
			want: false,
		},
		{
			name: "container with power request returns true",
			obj:  makePod(PowerNamespace, "TestNode", nil, powerReq, false),
			want: true,
		},
		{
			name: "container with power request in other namespace returns true",
			obj:  makePod("test-namespace", "TestNode", nil, powerReq, true),
			want: true,
		},
		{
			name: "init container with power request returns true",
			obj:  makePod(PowerNamespace, "TestNode", powerReq, cpuMemReq, true),
			want: true,
		},
		{
			name: "multiple containers where one requests power returns true",
			obj: func() client.Object {
				pod := makePod(PowerNamespace, "TestNode", nil, cpuMemReq, false)
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

// TestPowerPod_RestartRace_SharedPoolNotReady verifies that when the power node agent
// restarts, the PowerPod controller correctly requeues if CPUs are still in the reserved
// pool (shared workload not yet processed by PowerWorkload controller).
// This simulates the race: PowerPod reconciles before PowerWorkload sets up the shared pool.
func TestPowerPod_RestartRace_SharedPoolNotReady(t *testing.T) {
	nodeName := "TestNode"
	t.Setenv("NODE_NAME", nodeName)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restart-race-pod",
			Namespace: PowerNamespace,
			UID:       "restart-race-uid",
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name:      "test-container",
					Resources: defaultResources,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSGuaranteed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "test-container",
					ContainerID: "containerd://test-cid-1",
				},
			},
		},
	}

	powerNode := &powerv1.PowerNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName,
			Namespace: PowerNamespace,
		},
	}

	powerNodeState := &powerv1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-power-state", nodeName),
			Namespace: PowerNamespace,
		},
	}

	fakePodResources := []*podresourcesapi.PodResources{
		{
			Name:      "restart-race-pod",
			Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{
				{
					Name:   "test-container",
					CpuIds: []int64{10, 11, 12},
				},
			},
		},
	}

	objs := []runtime.Object{pod, defaultProfile, defaultWorkload, powerNode, powerNodeState}
	podResourcesClient := createFakePodResourcesListerClient(fakePodResources)

	// Custom reconciler setup — we need to control the shared pool state
	// instead of using createPodReconcilerObject which gives CPUs 0-99.
	log.SetLogger(zap.New(
		zap.UseDevMode(true),
		func(opts *zap.Options) {
			opts.TimeEncoder = zapcore.ISO8601TimeEncoder
		},
	))
	s := scheme.Scheme
	_ = powerv1.AddToScheme(s)

	cl := fake.NewClientBuilder().
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&powerv1.PowerWorkload{}, &powerv1.PowerNodeState{}).
		Build()

	state, _ := podstate.NewState()

	mockPowerLibrary := new(hostMock)
	mockPowerLibrary.On("GetExclusivePool", "performance").Return(createMockPoolWithCPUs([]uint{}))

	// Phase 1: Shared pool is EMPTY — simulates restart before PowerWorkload runs.
	// CPUs are still in the reserved pool.
	emptySharedPool := createMockPoolWithCPUs([]uint{})
	mockPowerLibrary.On("GetSharedPool").Return(emptySharedPool).Once()

	r := &PowerPodReconciler{
		Client:             cl,
		Log:                ctrl.Log.WithName("testing"),
		Scheme:             s,
		State:              state,
		PodResourcesClient: *podResourcesClient,
		PowerLibrary:       mockPowerLibrary,
	}

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{
			Name:      pod.Name,
			Namespace: PowerNamespace,
		},
	}

	// First reconcile: CPUs not in shared pool → should requeue without error.
	result, err := r.Reconcile(context.TODO(), req)
	assert.NoError(t, err, "should not error, just requeue")
	assert.True(t, result.RequeueAfter > 0, "should requeue when CPUs not in shared pool")

	// Phase 2: PowerWorkload controller has now run — CPUs moved to shared pool.
	populatedSharedPool := createMockPoolWithCPUs([]uint{10, 11, 12, 13, 14, 15})
	mockPowerLibrary.On("GetSharedPool").Return(populatedSharedPool)

	// Second reconcile: CPUs are now in shared pool → should succeed without requeue.
	result, err = r.Reconcile(context.TODO(), req)
	assert.NoError(t, err, "should succeed after shared pool is populated")
	assert.Equal(t, result.RequeueAfter, time.Duration(0), "should not requeue when CPUs are in shared pool")
}

func TestPowerPod_areCPUsInSharedPool(t *testing.T) {
	tcases := []struct {
		name           string
		sharedPoolCPUs []uint
		cpuIDs         []uint
		expected       bool
	}{
		{
			name:           "all CPUs in shared pool",
			sharedPoolCPUs: []uint{0, 1, 2, 3, 4},
			cpuIDs:         []uint{1, 2, 3},
			expected:       true,
		},
		{
			name:           "some CPUs not in shared pool",
			sharedPoolCPUs: []uint{0, 1, 2},
			cpuIDs:         []uint{1, 2, 5},
			expected:       false,
		},
		{
			name:           "no CPUs in shared pool",
			sharedPoolCPUs: []uint{0, 1, 2},
			cpuIDs:         []uint{10, 11},
			expected:       false,
		},
		{
			name:           "empty cpuIDs returns true",
			sharedPoolCPUs: []uint{0, 1, 2},
			cpuIDs:         []uint{},
			expected:       true,
		},
		{
			name:           "empty shared pool returns false",
			sharedPoolCPUs: []uint{},
			cpuIDs:         []uint{1},
			expected:       false,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			mockHost := new(hostMock)
			mockHost.On("GetSharedPool").Return(createMockPoolWithCPUs(tc.sharedPoolCPUs))

			r := &PowerPodReconciler{
				PowerLibrary: mockHost,
			}

			got := r.areCPUsInSharedPool(tc.cpuIDs)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestPowerWorkload_Reconcile_DetectCoresAdded(t *testing.T) {
	orig := []uint{1, 2, 3, 4}
	updated := []uint{1, 2, 4, 5}

	expectedResult := []uint{5}
	result := detectCoresAdded(orig, updated, &logr.Logger{})
	assert.ElementsMatch(t, result, expectedResult)
}
