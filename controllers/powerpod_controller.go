/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	e "errors"

	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift-kni/kubernetes-power-manager/pkg/podresourcesclient"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podstate"
)

const (
	PowerProfileAnnotation = "PowerProfile"
	ResourcePrefix         = "power.openshift.io/"
	CPUResource            = "cpu"
	PowerNamespace         = "power-manager"
)

// PowerPodReconciler reconciles a Pod object
type PowerPodReconciler struct {
	client.Client
	Log                logr.Logger
	Scheme             *runtime.Scheme
	State              *podstate.State
	PodResourcesClient podresourcesclient.PodResourcesClient
	PowerLibrary       power.Host
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.openshift.io,resources=powerworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=power.openshift.io,resources=powerprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=power.openshift.io,resources=powernodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

func (r *PowerPodReconciler) Reconcile(c context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("powerpod", req.NamespacedName)
	logger.Info("Reconciling the pod")

	pod := &corev1.Pod{}
	logger.V(5).Info("retrieving pod instance")
	err := r.Get(context.TODO(), req.NamespacedName, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Delete the Pod from the internal state in case it was never deleted
			// aAdded the check due to golangcilint errcheck
			_ = r.State.DeletePodFromState(req.NamespacedName.Name, req.NamespacedName.Namespace)
			return ctrl.Result{}, nil
		}

		logger.Error(err, "error while trying to retrieve the pod")
		return ctrl.Result{}, err
	}

	// The NODE_NAME environment variable is passed in via the downwards API in the pod spec
	nodeName := os.Getenv("NODE_NAME")

	if !pod.ObjectMeta.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodSucceeded {
		// If the pod's deletion timestamp is not zero, then the pod has been deleted

		// Delete the Pod from the internal state in case it was never deleted
		_ = r.State.DeletePodFromState(pod.GetName(), pod.GetNamespace())

		// Get the pod's CPU assignments from PowerNodeState to know what to clean up.
		powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)
		currentNodeState := &powerv1.PowerNodeState{}

		// Get the PowerNodeState to find this pod's CPU assignments and move them back to the shared pool.
		err = r.Get(context.TODO(), client.ObjectKey{Namespace: PowerNamespace, Name: powerNodeStateName}, currentNodeState)
		if err != nil {
			if errors.IsNotFound(err) {
				logger.Info("WARNING: PowerNodeState not found during pod deletion, CPUs may remain in exclusive pool", "powerNodeState", powerNodeStateName)
			} else {
				logger.Error(err, "failed to get PowerNodeState")
				return ctrl.Result{}, err
			}
		} else {
			// Move the pod's CPUs back to the shared pool.
			for _, exclusive := range currentNodeState.Status.CPUPools.Exclusive {
				if exclusive.PodUID == string(pod.GetUID()) {
					for _, container := range exclusive.PowerContainers {
						logger.V(5).Info("moving CPUs back to shared pool", "container", container.Name, "profile", container.PowerProfile, "cpus", container.CPUIDs)
						if err := r.PowerLibrary.GetSharedPool().MoveCpuIDs(container.CPUIDs); err != nil {
							logger.Error(err, "failed to move CPUs back to shared pool", "container", container.Name, "profile", container.PowerProfile)
							return ctrl.Result{}, err
						}
					}
					break
				}
			}
		}

		// Remove the pod's entry from PowerNodeState via SSA (passing empty containers).
		if err := r.updatePowerNodeStateExclusiveStatus(nodeName, string(pod.GetUID()), pod.Name, nil, &logger); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// If the pod's deletion timestamp is equal to zero, then the pod has been created or updated.

	// Make sure the pod is running
	logger.V(5).Info("confirming the pod is in a running state")
	podNotRunningErr := errors.NewServiceUnavailable("the pod is not in the running phase")
	if pod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, podNotRunningErr
	}

	// Get customDevices that need to be considered in the pod.
	logger.V(5).Info("retrieving custom resources from power node")
	powernode := &powerv1.PowerNode{}
	err = r.Get(context.TODO(), client.ObjectKey{
		Namespace: PowerNamespace,
		Name:      nodeName,
	}, powernode)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "error while trying to retrieve the power node")
		return ctrl.Result{}, err
	}

	// Get the Containers of the Pod that are requesting exclusive CPUs or custom devices.
	logger.V(5).Info("retrieving the containers requested for the exclusive CPUs or Custom Resources", "Custom Resources", powernode.Status.CustomDevices)
	admissibleContainers := getAdmissibleContainers(pod, powernode.Status.CustomDevices, r.PodResourcesClient, &logger)
	if len(admissibleContainers) == 0 {
		logger.Info("no containers are requesting exclusive CPUs or Custom Resources")
		return ctrl.Result{}, nil
	}
	podUID := pod.GetUID()
	logger.V(5).Info("retrieving the podUID", "UID", podUID)
	if podUID == "" {
		logger.Info("no pod UID found")
		return ctrl.Result{}, errors.NewServiceUnavailable("pod UID not found")
	}

	// Get the power containers requested by containers in the pod.
	powerContainers, recoveryErrs := r.getPowerProfileRequestsFromContainers(admissibleContainers, powernode.Status.CustomDevices, pod, &logger)
	logger.V(5).Info("retrieved power profiles and containers from pod requests")

	// Reconcile CPU pools and track errors per container.
	// Skip containers that already have errors (e.g., profile unavailability).
	for i := range powerContainers {
		container := &powerContainers[i]
		// Skip CPU pool reconciliation for containers with existing errors.
		if len(container.Errors) > 0 {
			logger.V(5).Info("skipping CPU pool reconciliation for container with errors", "container", container.Name)
			continue
		}

		exclusivePool := r.PowerLibrary.GetExclusivePool(container.PowerProfile)
		if exclusivePool == nil {
			err := fmt.Errorf("exclusive pool for profile %s not found", container.PowerProfile)
			logger.Error(err, "failed to get exclusive pool", "container", container.Name)
			container.Errors = append(container.Errors, err.Error())
			recoveryErrs = append(recoveryErrs, err)
			continue
		}

		// Get actual CPUs currently in the exclusive pool.
		actualCPUs := exclusivePool.Cpus().IDs()

		// Compute delta: cores to add (in desired but not in actual).
		coresToAdd := detectCoresAdded(actualCPUs, container.CPUIDs, &logger)
		if len(coresToAdd) > 0 {
			// CPUs can only be moved to exclusive pool from shared pool.
			// If CPUs are still in the reserved pool, the shared workload hasn't been processed yet - requeue and wait for it.
			if !r.areCPUsInSharedPool(coresToAdd) {
				logger.Info("CPUs not yet in shared pool, waiting for shared workload to be processed", "cpus", coresToAdd)
				return ctrl.Result{RequeueAfter: queuetime}, nil
			}

			logger.V(5).Info("moving CPUs to exclusive pool", "profile", container.PowerProfile, "container", container.Name, "cpus", coresToAdd)
			if err := exclusivePool.MoveCpuIDs(coresToAdd); err != nil {
				logger.Error(err, "failed to move CPUs to exclusive pool", "profile", container.PowerProfile, "container", container.Name)
				container.Errors = append(container.Errors, err.Error())
				recoveryErrs = append(recoveryErrs, err)
			}
		}
	}

	// Update PowerNodeState status with container info (errors are already on each PowerContainer).
	if err := r.updatePowerNodeStateExclusiveStatus(nodeName, string(podUID), pod.Name, powerContainers, &logger); err != nil {
		return ctrl.Result{}, err
	}

	// Finally, update the controller's state
	logger.V(5).Info("updating the controller's internal state")
	guaranteedPod := powerv1.GuaranteedPod{}
	guaranteedPod.Node = pod.Spec.NodeName
	guaranteedPod.Name = pod.GetName()
	guaranteedPod.Namespace = pod.Namespace
	guaranteedPod.UID = string(podUID)
	stateContainers := make([]powerv1.Container, 0, len(powerContainers))
	for _, pc := range powerContainers {
		stateContainers = append(stateContainers, powerv1.Container{
			Name:          pc.Name,
			Id:            pc.ID,
			PowerProfile:  pc.PowerProfile,
			ExclusiveCPUs: pc.CPUIDs,
		})
	}
	guaranteedPod.Containers = stateContainers
	err = r.State.UpdateStateGuaranteedPods(guaranteedPod)
	if err != nil {
		logger.Error(err, "error updating the internal state")
		return ctrl.Result{}, err
	}
	wrappedErrs := e.Join(recoveryErrs...)
	if wrappedErrs != nil {
		logger.Error(wrappedErrs, "recoverable errors")
		return ctrl.Result{Requeue: false}, fmt.Errorf("recoverable errors encountered: %w", wrappedErrs)
	}
	return ctrl.Result{}, nil
}

func (r *PowerPodReconciler) getPowerProfileRequestsFromContainers(containers []corev1.Container, customDevices []string, pod *corev1.Pod, logger *logr.Logger) ([]powerv1.PowerContainer, []error) {
	logger.V(5).Info("get the power profiles from the containers")
	var recoverableErrs []error
	powerContainers := make([]powerv1.PowerContainer, 0)
	for _, container := range containers {
		logger.V(5).Info("retrieving the requested power profile from the container spec")
		profileName, requestNum, err := getContainerProfileFromRequests(container, customDevices, logger)
		if err != nil {
			recoverableErrs = append(recoverableErrs, err)
			continue
		}

		// If there was no profile requested in this container we can move onto the next one
		if profileName == "" {
			logger.V(5).Info("no profile was requested by the container")
			continue
		}
		profileAvailable, err := validateProfileAvailabilityOnNode(context.TODO(), r.Client, profileName, pod.Spec.NodeName, r.PowerLibrary, logger)
		if err != nil {
			logger.Error(err, "error checking if power profile is available on node")
			continue
		}
		if !profileAvailable {
			errMsg := fmt.Sprintf("power profile '%s' is not available on node %s", profileName, pod.Spec.NodeName)
			recoverableErrs = append(recoverableErrs, errors.NewServiceUnavailable(errMsg))

			// Add the container with its error so it appears in PowerNodeState.
			// CPU pool reconciliation will be skipped for containers with errors.
			containerID := getContainerID(pod, container.Name)
			powerContainers = append(powerContainers, powerv1.PowerContainer{
				Name:         container.Name,
				ID:           strings.TrimPrefix(containerID, "docker://"),
				PowerProfile: profileName,
				CPUIDs:       []uint{},
				Errors:       []string{errMsg},
			})
			continue
		}
		containerID := getContainerID(pod, container.Name)
		coreIDs, err := r.PodResourcesClient.GetContainerCPUs(pod.GetName(), container.Name)
		if err != nil {
			logger.V(5).Info("error getting CoreIDs.", "ContainerID", containerID)
			recoverableErrs = append(recoverableErrs, err)
			continue
		}
		cleanCoreList := getCleanCoreList(coreIDs)
		logger.V(5).Info("reserving cores to container.", "ContainerID", containerID, "Cores", cleanCoreList)

		// Accounts for case where cores aquired through DRA don't match profile requests.
		if len(cleanCoreList) != requestNum {
			recoverableErrs = append(recoverableErrs, fmt.Errorf("assigned cores did not match requested profiles. cores:%d, profiles %d", len(cleanCoreList), requestNum))
			continue
		}
		logger.V(5).Info("creating the power container.", "ContainerID", containerID, "Cores", cleanCoreList, "Profile", profileName)
		powerContainers = append(powerContainers, powerv1.PowerContainer{
			Name:         container.Name,
			ID:           strings.TrimPrefix(containerID, "docker://"),
			CPUIDs:       cleanCoreList,
			PowerProfile: profileName,
		})
	}

	return powerContainers, recoverableErrs
}

func getContainerProfileFromRequests(container corev1.Container, customDevices []string, logger *logr.Logger) (string, int, error) {
	profileName := ""
	moreThanOneProfileError := errors.NewServiceUnavailable("cannot have more than one power profile per container")
	resourceRequestsMismatchError := errors.NewServiceUnavailable("mismatch between CPU requests and the power profile requests")
	for resource := range container.Resources.Requests {
		if strings.HasPrefix(string(resource), ResourcePrefix) {
			if profileName == "" {
				profileName = string(resource[len(ResourcePrefix):])
			} else {
				// Cannot have more than one profile for a singular container
				return "", 0, moreThanOneProfileError
			}
		}
	}
	var intProfileRequests int
	if profileName != "" {
		// Check if there is a mismatch in CPU requests and the power profile requests
		logger.V(5).Info("confirming that CPU requests and the power profiles request match")
		powerProfileResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ResourcePrefix, profileName))
		numRequestsPowerProfile := container.Resources.Requests[powerProfileResourceName]
		numLimitsPowerProfile := container.Resources.Limits[powerProfileResourceName]
		intProfileRequests = int(numRequestsPowerProfile.Value())
		intProfileLimits := int(numLimitsPowerProfile.Value())
		// Selecting resources to search
		numRequestsCPU := 0
		numLimitsCPU := 0

		// If the custom resource is requested, change the CPU request to
		// allow the check
		for _, deviceName := range customDevices {
			numRequestsCPU, numLimitsCPU = checkResource(container, corev1.ResourceName(deviceName), numRequestsCPU, numLimitsCPU)
		}

		if numRequestsCPU == 0 {
			numRequestsCPU, numLimitsCPU = checkResource(container, CPUResource, 0, 0)
		}

		// if previous checks fail we need to account for resource claims
		// if there's a problem with core numbers we'll catch it
		// before moving cores to pools by comparing intProfileRequests with assigned cores
		if numRequestsCPU == 0 && len(container.Resources.Claims) > 0 {
			return profileName, intProfileRequests, nil
		}
		if numRequestsCPU != intProfileRequests ||
			numLimitsCPU != intProfileLimits {
			return "", 0, resourceRequestsMismatchError
		}
	}

	return profileName, intProfileRequests, nil
}

func checkResource(container corev1.Container, resource corev1.ResourceName, numRequestsCPU int, numLimitsCPU int) (int, int) {
	numRequestsDevice := container.Resources.Requests[resource]
	numRequestsCPU += int(numRequestsDevice.Value())

	numLimitsDevice := container.Resources.Limits[resource]
	numLimitsCPU += int(numLimitsDevice.Value())
	return numRequestsCPU, numLimitsCPU
}

func getAdmissibleContainers(pod *corev1.Pod, customDevices []string, resourceClient podresourcesclient.PodResourcesClient, logger *logr.Logger) []corev1.Container {

	logger.V(5).Info("receiving containers requesting exclusive CPUs")
	admissibleContainers := make([]corev1.Container, 0)
	containerList := append(pod.Spec.InitContainers, pod.Spec.Containers...)
	controlPlaneAvailable := pingControlPlane(resourceClient)
	for _, container := range containerList {
		if doesContainerRequireExclusiveCPUs(pod, &container, logger) || validateCustomDevices(&container, customDevices) || controlPlaneAvailable {
			admissibleContainers = append(admissibleContainers, container)
		}
	}
	logger.V(5).Info("containers requesting exclusive resources are: ", "Containers", admissibleContainers)
	return admissibleContainers
}

func doesContainerRequireExclusiveCPUs(pod *corev1.Pod, container *corev1.Container, logger *logr.Logger) bool {
	if pod.Status.QOSClass != corev1.PodQOSGuaranteed {
		logger.V(3).Info(fmt.Sprintf("pod %s is not in guaranteed quality of service class", pod.Name))
		return false
	}

	cpuQuantity := container.Resources.Requests[corev1.ResourceCPU]
	return cpuQuantity.Value()*1000 == cpuQuantity.MilliValue()
}

func pingControlPlane(client podresourcesclient.PodResourcesClient) bool {
	// see if the socket sends a response
	req := podresourcesapi.ListPodResourcesRequest{}
	_, err := client.CpuControlPlaneClient.List(context.TODO(), &req)
	return err == nil
}

func validateCustomDevices(container *corev1.Container, customDevices []string) bool {
	presence := false
	for _, devicePlugin := range customDevices {
		numResources := container.Resources.Requests[corev1.ResourceName(devicePlugin)]
		if numResources.Value() > 0 {
			presence = numResources.Value()*1000 == numResources.MilliValue()
		}
	}
	return presence
}

func getContainerID(pod *corev1.Pod, containerName string) string {
	for _, containerStatus := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if containerStatus.Name == containerName {
			return containerStatus.ContainerID
		}
	}

	return ""
}

func getCleanCoreList(coreIDs string) []uint {
	cleanCores := make([]uint, 0)
	commaSeparated := strings.Split(coreIDs, ",")
	for _, splitCore := range commaSeparated {
		hyphenSeparated := strings.Split(splitCore, "-")
		if len(hyphenSeparated) == 1 {
			intCore, err := strconv.ParseUint(hyphenSeparated[0], 10, 32)
			if err != nil {
				fmt.Printf("error getting the core list: %v", err)
				return []uint{}
			}
			cleanCores = append(cleanCores, uint(intCore))
		} else {
			startCore, err := strconv.Atoi(hyphenSeparated[0])
			if err != nil {
				fmt.Printf("error getting the core list: %v", err)
				return []uint{}
			}
			endCore, err := strconv.Atoi(hyphenSeparated[len(hyphenSeparated)-1])
			if err != nil {
				fmt.Printf("error getting the core list: %v", err)
				return []uint{}
			}
			for i := startCore; i <= endCore; i++ {
				cleanCores = append(cleanCores, uint(i))
			}
		}
	}

	return cleanCores
}

// PowerReleventPodPredicate returns true if this pod should be considered by the controller
// based on node scope and presence of power profile resource requests.
func PowerReleventPodPredicate(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	if pod.Spec.NodeName != os.Getenv("NODE_NAME") {
		return false
	}
	containers := append(pod.Spec.InitContainers, pod.Spec.Containers...)
	for _, c := range containers {
		// No need to check the limits, as if the requests are present,
		// the limits must be present as well.
		for rn := range c.Resources.Requests {
			if strings.HasPrefix(string(rn), ResourcePrefix) {
				return true
			}
		}
	}
	return false
}

// updatePowerNodeStateExclusiveStatus updates the PowerNodeState status with exclusive CPU pool
// information for this pod's containers, including any errors encountered.
// When powerContainers is empty, it removes the pod's entry from PowerNodeState via SSA.
func (r *PowerPodReconciler) updatePowerNodeStateExclusiveStatus(
	nodeName string,
	podUID string,
	podName string,
	powerContainers []powerv1.PowerContainer,
	logger *logr.Logger,
) error {
	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)
	fieldManager := fmt.Sprintf("powerpod-controller.%s", podUID)

	var exclusiveStatus []powerv1.ExclusiveCPUPoolStatus

	if len(powerContainers) > 0 {
		exclusiveStatus = []powerv1.ExclusiveCPUPoolStatus{
			{
				PodUID:          podUID,
				Pod:             podName,
				PowerContainers: powerContainers,
			},
		}
	} else {
		// Empty list removes this pod's entry via SSA.
		exclusiveStatus = []powerv1.ExclusiveCPUPoolStatus{}
	}

	patchNodeState := &powerv1.PowerNodeState{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "power.openshift.io/v1",
			Kind:       "PowerNodeState",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      powerNodeStateName,
			Namespace: PowerNamespace,
		},
		Status: powerv1.PowerNodeStateStatus{
			CPUPools: powerv1.CPUPoolsStatus{
				// Reserved is left nil (omitted via omitempty) so this controller
				// doesn't claim SSA ownership of the atomic Reserved list.
				Exclusive: exclusiveStatus,
			},
		},
	}

	logger.V(5).Info("updating PowerNodeState status with SSA", "fieldManager", fieldManager)
	if err := r.Status().Patch(context.TODO(), patchNodeState, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("PowerNodeState not found, skipping exclusive pool status update", "powerNodeState", powerNodeStateName)
			return nil
		}
		logger.Error(err, "failed to update PowerNodeState status")
		return err
	}

	return nil
}

// areCPUsInSharedPool checks if all specified CPUs are currently in the shared pool.
// Returns false if any CPU is still in the reserved pool (shared workload not yet processed).
func (r *PowerPodReconciler) areCPUsInSharedPool(cpuIDs []uint) bool {
	sharedPool := r.PowerLibrary.GetSharedPool()
	sharedCPUIDs := sharedPool.Cpus().IDs()

	for _, cpuID := range cpuIDs {
		found := false
		for _, sharedCPUID := range sharedCPUIDs {
			if cpuID == sharedCPUID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (r *PowerPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{},
			builder.WithPredicates(
				predicate.NewPredicateFuncs(PowerReleventPodPredicate))).
		Complete(r)
}
