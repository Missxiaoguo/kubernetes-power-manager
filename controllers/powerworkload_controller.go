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
	"strings"

	e "errors"

	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/openshift-kni/kubernetes-power-manager/internal/scaling"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/util"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
)

// PowerWorkloadReconciler reconciles a PowerWorkload object
type PowerWorkloadReconciler struct {
	client.Client
	Log                 logr.Logger
	Scheme              *runtime.Scheme
	PowerLibrary        power.Host
	DPDKTelemetryClient scaling.DPDKTelemetryClient
	CPUScalingManager   scaling.CPUScalingManager
}

type dpdkTelemetryConfiguration struct {
	current *scaling.DPDKTelemetryConnectionData
	new     *scaling.DPDKTelemetryConnectionData
}

const (
	SharedWorkloadName string = "shared-workload"
	WorkloadNameSuffix string = "-workload"
)

var sharedPowerWorkloadName = ""

// +kubebuilder:rbac:groups=power.intel.com,resources=powerworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=power.intel.com,resources=powerworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

func (r *PowerWorkloadReconciler) Reconcile(c context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("powerworkload", req.NamespacedName)
	logger.Info("Reconciling the power workload")

	var err error
	if req.Namespace != IntelPowerNamespace {
		err := fmt.Errorf("incorrect namespace")
		logger.Error(err, "resource is not in the intel-power namespace, ignoring")
		return ctrl.Result{Requeue: false}, err
	}

	nodeName := os.Getenv("NODE_NAME")

	workload := &powerv1.PowerWorkload{}
	defer func() { _ = writeUpdatedStatusErrsIfRequired(c, r.Status(), workload, err) }()

	err = r.Client.Get(context.TODO(), req.NamespacedName, workload)
	logger.V(5).Info("retrieving the power workload instance")
	if err != nil {
		if errors.IsNotFound(err) {
			// If the profile still exists in the power library, then only the power workload was deleted
			// and we need to remove it from the power library here. If the profile doesn't exist, then
			// the power library will have deleted it for us
			if req.NamespacedName.Name == sharedPowerWorkloadName {
				movedCores := *r.PowerLibrary.GetSharedPool().Cpus()
				pools := r.PowerLibrary.GetAllExclusivePools()
				for _, pool := range *pools {
					if strings.Contains(pool.Name(), nodeName+"-reserved-") {
						movedCores = append(movedCores, *pool.Cpus()...)
						if err := pool.Remove(); err != nil {
							logger.Error(err, "failed to remove reserved pool")
							return ctrl.Result{}, err
						}
					}
				}
				err = r.PowerLibrary.GetReservedPool().MoveCpus(movedCores)
				if err != nil {
					logger.Error(err, "failed to return all non exclusive cores to default reserved pool")
					return ctrl.Result{}, err
				}
				sharedPowerWorkloadName = ""
			} else {
				pool := r.PowerLibrary.GetExclusivePool(strings.ReplaceAll(req.NamespacedName.Name, ("-" + nodeName), ""))
				if pool != nil {
					err = pool.Remove()
					if err != nil {
						logger.Error(err, "failed to remove the exclusive pool")
						return ctrl.Result{}, err
					}
				}
			}

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	// Set the name of the workload node for non shared PowerWorkloads.
	workload.Status.WorkloadNodes.Name = nodeName
	err = r.Client.Status().Update(context.TODO(), workload)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If there are multiple nodes the shared power workload's node selector satisfies we need to fail here before anything is done
	logger.V(5).Info("checking the node elector is satisfied with the shared power workload")
	if workload.Spec.AllCores {
		labelledNodeList := &corev1.NodeList{}
		listOption := workload.Spec.PowerNodeSelector

		err = r.Client.List(context.TODO(), labelledNodeList, client.MatchingLabels(listOption))
		if err != nil {
			logger.Error(err, "error retrieving the node with the power node selector", "selector", listOption)
			return ctrl.Result{}, err
		}

		// If there were no nodes that matched the provided labels, check the node info of the workload for a name
		logger.V(5).Info("checking the node info to see if the node name has been provided")
		if (len(labelledNodeList.Items) == 0 && workload.Status.WorkloadNodes.Name != nodeName) || !util.NodeNameInNodeList(nodeName, labelledNodeList.Items) {
			return ctrl.Result{}, nil
		}

		logger.V(5).Info("verifying there is only one shared power workload and if there is more than one delete this instance")
		if sharedPowerWorkloadName != "" && sharedPowerWorkloadName != req.NamespacedName.Name {
			// Delete this shared power workload as another already exists
			err = r.Client.Delete(context.TODO(), workload)
			if err != nil {
				logger.Error(err, "error deleting the second shared power workload")
				return ctrl.Result{}, err
			}

			sharedPowerWorkloadAlreadyExists := errors.NewServiceUnavailable("a shared power workload already exists for this node")
			logger.Error(sharedPowerWorkloadAlreadyExists, "error creating the shared power workload")
			return ctrl.Result{Requeue: false}, sharedPowerWorkloadAlreadyExists
		}

		logger.V(5).Info("verifying the requested power profile(s) exist and are available on this node")
		requestedProfiles := []string{workload.Spec.PowerProfile}
		for _, coreConfig := range workload.Spec.ReservedCPUs {
			requestedProfiles = append(requestedProfiles, coreConfig.PowerProfile)
		}
		for _, profileName := range requestedProfiles {
			match, localerr := validateProfileAvailabilityOnNode(c, r.Client, profileName, nodeName, r.PowerLibrary, &logger)
			if localerr != nil {
				if errors.IsNotFound(localerr) {
					logger.Error(localerr, "power profile not found, will retry")
					err = fmt.Errorf("requested PowerProfile '%s' does not exist in the cluster", profileName)
					return ctrl.Result{RequeueAfter: queuetime}, nil
				}
				return ctrl.Result{}, localerr
			}
			if !match {
				logger.Error(fmt.Errorf("power profile not available on the node"), fmt.Sprintf("requested PowerProfile '%s' is not available on node %s", profileName, nodeName))
				err = fmt.Errorf("requested PowerProfile '%s' is not available on the node %s", profileName, nodeName)
				return ctrl.Result{RequeueAfter: queuetime}, nil
			}
		}

		// retrieve pool with the profile we want to attach to the shared pool
		pool := r.PowerLibrary.GetExclusivePool(workload.Spec.PowerProfile)
		if pool == nil {
			logger.Error(fmt.Errorf("pool not found"), fmt.Sprintf("could not retrieve pool for profile %s", workload.Spec.PowerProfile))
			return ctrl.Result{Requeue: false}, fmt.Errorf("pool not found")
		}
		profile := pool.GetPowerProfile()
		// shouldn't be possible but just in case
		if profile == nil {
			logger.Error(fmt.Errorf("pool not found"), fmt.Sprintf("pool  %s did not have the subsequent profile", workload.Spec.PowerProfile))
			return ctrl.Result{Requeue: false}, fmt.Errorf("profile not found")
		}
		err = r.PowerLibrary.GetSharedPool().SetPowerProfile(profile)
		if err != nil {
			logger.Error(err, "could not set the power profile for the shared pool")
			return ctrl.Result{Requeue: false}, err
		}

		// move all cores to the shared pool,
		// then set up individual pools for reserved cores
		logger.V(5).Info("creating the shared pool in the power library")
		if err := r.PowerLibrary.GetReservedPool().SetCpuIDs([]uint{}); err != nil {
			logger.Error(err, "error initializing reserved pool")
			return ctrl.Result{}, err
		}
		// remove the existing reserved pools in case they aren't needed after this
		pools := r.PowerLibrary.GetAllExclusivePools()
		for _, pool := range *pools {
			if strings.Contains(pool.Name(), nodeName+"-reserved-") {
				if err := pool.Remove(); err != nil {
					logger.Error(err, "failed to remove reserved pool")
					return ctrl.Result{}, err
				}
			}
		}
		var recoveryErrs []error
		for _, coreConfig := range workload.Spec.ReservedCPUs {
			// move cores to shared pool to prevent exclusive->reserved conflicts
			if err := r.PowerLibrary.GetSharedPool().MoveCpuIDs(coreConfig.Cores); err != nil {
				logger.Error(err, "error moving cores to shared pool")
				return ctrl.Result{}, err
			}
			if coreConfig.PowerProfile != "" {
				if err := createReservedPool(r.PowerLibrary, coreConfig, &logger); err != nil {
					recoveryErrs = append(recoveryErrs, err)
					// if attaching a profile failed, try moving the cores to the default reserved pool
					if err := r.PowerLibrary.GetReservedPool().MoveCpuIDs(coreConfig.Cores); err != nil {
						logger.Error(err, "error moving cores to reserved pool")
						return ctrl.Result{}, err
					}
				}
			} else {
				// no profile specified so leave the cores in the default reserved pool
				if err := r.PowerLibrary.GetReservedPool().MoveCpuIDs(coreConfig.Cores); err != nil {
					logger.Error(err, "error moving cores to reserved pool")
					return ctrl.Result{}, err
				}
			}
		}
		sharedPowerWorkloadName = req.NamespacedName.Name
		wrappedErrs := e.Join(recoveryErrs...)
		if wrappedErrs != nil {
			errString := "error(s) encountered establishing reserved pool"
			logger.Error(wrappedErrs, errString)
			return ctrl.Result{Requeue: false}, e.New(errString)
		}
		return ctrl.Result{}, nil
	}

	// Handle exclusive workload
	if workload.Status.WorkloadNodes.Name == nodeName {
		poolFromLibrary := r.PowerLibrary.GetExclusivePool(workload.Spec.PowerProfile)
		if poolFromLibrary == nil {
			poolDoesNotExistError := errors.NewServiceUnavailable(fmt.Sprintf("pool '%s' does not exist in the power library", workload.Spec.PowerProfile))
			logger.Error(poolDoesNotExistError, "error retrieving the pool from the power library")
			return ctrl.Result{Requeue: false}, poolDoesNotExistError
		}

		logger.V(5).Info("updating the CPU list in the power library")
		cores := poolFromLibrary.Cpus().IDs()
		coresToRemoveFromLibrary := detectCoresRemoved(cores, workload.Status.WorkloadNodes.CpuIds, &logger)
		coresToBeAddedToLibrary := detectCoresAdded(cores, workload.Status.WorkloadNodes.CpuIds, &logger)

		if len(coresToRemoveFromLibrary) > 0 {
			err = r.PowerLibrary.GetSharedPool().MoveCpuIDs(coresToRemoveFromLibrary)
			if err != nil {
				logger.Error(err, "error updating the power library CPU list")
				return ctrl.Result{}, err
			}
		}

		if len(coresToBeAddedToLibrary) > 0 {
			err = r.PowerLibrary.GetExclusivePool(workload.Spec.PowerProfile).MoveCpuIDs(coresToBeAddedToLibrary)
			if err != nil {
				logger.Error(err, "error updating the power library CPU list")
				return ctrl.Result{}, err
			}
		}

		// If the referenced PowerProfile defines a CPU scaling policy for DPDK polling
		// workloads, delegate to the CPU scaling manager. It will dynamically tune
		// per-CPU frequencies based on usage samples retrieved from DPDK telemetry
		// when needed.
		profile := &powerv1.PowerProfile{}
		err = r.Client.Get(c, client.ObjectKey{
			Namespace: IntelPowerNamespace,
			Name:      workload.Spec.PowerProfile,
		}, profile)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get power profile: %w", err)
		}
		if profile.Spec.CpuScalingPolicy != nil && profile.Spec.CpuScalingPolicy.WorkloadType == "polling-dpdk" {
			// Ensure telemetry connections for all DPDK pods in this workload.
			r.reconcileDPDKTelemetryClient(workload.Status.WorkloadNodes.Containers)

			// Build scaling options for each CPU in this exclusive pool.
			cpus := r.PowerLibrary.GetExclusivePool(workload.Spec.PowerProfile).Cpus()
			scalingOpts := r.generateCPUScalingOpts(profile.Spec.CpuScalingPolicy, cpus)

			// Hand over to the scaling manager to manage per-CPU scaling.
			r.CPUScalingManager.ManageCPUScaling(scalingOpts)
		}
	}

	return ctrl.Result{}, nil
}

func detectCoresRemoved(originalCoreList []uint, updatedCoreList []uint, logger *logr.Logger) []uint {
	var coresRemoved []uint
	logger.V(5).Info("detecting if cores are removed from the cores list")
	for _, core := range originalCoreList {
		if !validateCoreIsInCoreList(core, updatedCoreList) {
			coresRemoved = append(coresRemoved, core)
		}
	}

	return coresRemoved
}

func detectCoresAdded(originalCoreList []uint, updatedCoreList []uint, logger *logr.Logger) []uint {
	var coresAdded []uint
	logger.V(5).Info("detecting if cores are added to the cores list")
	for _, core := range updatedCoreList {
		if !validateCoreIsInCoreList(core, originalCoreList) {
			coresAdded = append(coresAdded, core)
		}
	}

	return coresAdded
}

func validateCoreIsInCoreList(core uint, coreList []uint) bool {
	for _, c := range coreList {
		if c == core {
			return true
		}
	}

	return false
}

func createReservedPool(library power.Host, coreConfig powerv1.ReservedSpec, logger *logr.Logger) error {
	pseudoReservedPool, err := library.AddExclusivePool(os.Getenv("NODE_NAME") + "-reserved-" + fmt.Sprintf("%v", coreConfig.Cores))
	if err != nil {
		logger.Error(err, fmt.Sprintf("error creating reserved pool for cores %v", coreConfig.Cores))
		return err
	}

	corePool := library.GetExclusivePool(coreConfig.PowerProfile)
	if corePool == nil {
		if removePoolError := pseudoReservedPool.Remove(); removePoolError != nil {
			logger.Error(removePoolError, fmt.Sprintf("error removing pool %v", pseudoReservedPool.Name()))
		}

		logger.Error(err, "error setting retrieving exclusive pool for reserved cores")
		return fmt.Errorf("specified profile %s has no existing pool", coreConfig.PowerProfile)
	}
	if err := pseudoReservedPool.SetPowerProfile(corePool.GetPowerProfile()); err != nil {
		if removePoolError := pseudoReservedPool.Remove(); removePoolError != nil {
			logger.Error(removePoolError, fmt.Sprintf("error removing pool %v", pseudoReservedPool.Name()))
		}
		logger.Error(err, "error setting profile for reserved cores")
		return err
	}

	if err := pseudoReservedPool.SetCpuIDs(coreConfig.Cores); err != nil {
		if removePoolError := pseudoReservedPool.Remove(); removePoolError != nil {
			logger.Error(removePoolError, fmt.Sprintf("error removing pool %v", pseudoReservedPool.Name()))
		}

		logger.Error(err, "error moving cores to special reserved pool")
		return err
	}

	return nil
}

// reconcileDPDKTelemetryClient manages the lifecycle of DPDK telemetry
// connections. It maintains one connection per pod and each connection
// reports the usage samples(busy cycles and total cycles) for the CPUs
// managed by that pod.
func (r *PowerWorkloadReconciler) reconcileDPDKTelemetryClient(containers []powerv1.Container) {
	// gather current connection configurations
	dpdkConfigMap := map[string]*dpdkTelemetryConfiguration{}
	for _, connData := range r.DPDKTelemetryClient.ListConnections() {
		dpdkConfigMap[connData.PodUID] = &dpdkTelemetryConfiguration{
			current: &connData,
			new:     nil,
		}
	}

	// gather incoming connection configurations and group them based on pod UID
	for _, container := range containers {
		podUID := string(container.PodUID)
		cpuIDs := container.ExclusiveCPUs

		if dpdkConfig, found := dpdkConfigMap[podUID]; found {
			if dpdkConfig.new == nil {
				dpdkConfig.new = &scaling.DPDKTelemetryConnectionData{
					PodUID:      podUID,
					WatchedCPUs: cpuIDs,
				}
			} else {
				dpdkConfig.new.WatchedCPUs = append(dpdkConfig.new.WatchedCPUs, cpuIDs...)
			}
		} else {
			dpdkConfigMap[podUID] = &dpdkTelemetryConfiguration{
				current: nil,
				new: &scaling.DPDKTelemetryConnectionData{
					PodUID:      podUID,
					WatchedCPUs: cpuIDs,
				},
			}
		}
	}

	// NOTE: Exclusive CPUs for containers are fixed
	// at Pod scheduling and don't change.
	// Thus, we can skip updating the CPU list post-connection.
	for podUID, dpdkConfig := range dpdkConfigMap {
		if dpdkConfig.new == nil {
			r.DPDKTelemetryClient.CloseConnection(podUID)
			continue
		}
		if dpdkConfig.current == nil {
			r.DPDKTelemetryClient.CreateConnection(dpdkConfig.new)
		}
	}
}

// generateCPUScalingOpts translates a CpuScalingPolicy and a set of CPUs
// into a per-CPU list of scaling options used by the CPUScalingManager.
func (r *PowerWorkloadReconciler) generateCPUScalingOpts(scalingPolicy *powerv1.CpuScalingPolicy, cpus *power.CpuList) []scaling.CPUScalingOpts {
	optsList := make([]scaling.CPUScalingOpts, 0)

	for _, cpu := range *cpus {
		// Get the min and max frequency of this CPU and calculate the fallback frequency.
		minFreq, maxFreq := cpu.GetAbsMinMax()
		fallbackFreqPct := uint(*scalingPolicy.FallbackFreqPercent)
		fallbackFreq := minFreq + (maxFreq-minFreq)*(fallbackFreqPct)/100

		opts := scaling.CPUScalingOpts{
			CPU:                        cpu,
			SamplePeriod:               scalingPolicy.SamplePeriod.Duration,
			CooldownPeriod:             scalingPolicy.CooldownPeriod.Duration,
			TargetUsage:                *scalingPolicy.TargetUsage,
			AllowedUsageDifference:     *scalingPolicy.AllowedUsageDifference,
			AllowedFrequencyDifference: *scalingPolicy.AllowedFrequencyDifference * 1000,
			HWMaxFrequency:             int(maxFreq),
			HWMinFrequency:             int(minFreq),
			CurrentTargetFrequency:     scaling.FrequencyNotYetSet,
			ScaleFactor:                float64(*scalingPolicy.ScalePercentage) / 100.0,
			FallbackFreq:               int(fallbackFreq),
		}
		optsList = append(optsList, opts)
	}

	return optsList
}

func (r *PowerWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&powerv1.PowerWorkload{}).
		Watches(&powerv1.PowerProfile{},
			handler.EnqueueRequestsFromMapFunc(r.powerProfileToWorkloadRequests),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					oldProfile := e.ObjectOld.(*powerv1.PowerProfile)
					newProfile := e.ObjectNew.(*powerv1.PowerProfile)

					// Filter out shared profiles.
					if newProfile.Spec.Shared {
						return false
					}

					// Filter for CPU scaling policy changes only.
					return oldProfile.Spec.CpuScalingPolicy != newProfile.Spec.CpuScalingPolicy
				},
				CreateFunc:  func(e event.CreateEvent) bool { return false },
				GenericFunc: func(ge event.GenericEvent) bool { return false },
				DeleteFunc:  func(de event.DeleteEvent) bool { return false },
			})).
		Complete(r)
}

// powerProfileToWorkloadRequests enqueues reconciliation requests for the
// PowerWorkload that is associated with the given PowerProfile.
func (r *PowerWorkloadReconciler) powerProfileToWorkloadRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	powerProfile := obj.(*powerv1.PowerProfile)
	requests := []reconcile.Request{}

	workloadName := fmt.Sprintf("%s-%s", powerProfile.Name, os.Getenv("NODE_NAME"))
	workload := &powerv1.PowerWorkload{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      workloadName,
		Namespace: IntelPowerNamespace,
	}, workload); err != nil {
		if errors.IsNotFound(err) {
			return requests
		}
		r.Log.Error(err, "failed to get power workload")
		return requests
	}
	if workload.Spec.PowerProfile != powerProfile.Name {
		return requests
	}

	requests = append(requests, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      workloadName,
			Namespace: IntelPowerNamespace,
		},
	})
	r.Log.V(5).Info("Enqueuing PowerWorkload reconciliation requests", "powerProfile", powerProfile.Name)
	return requests
}
