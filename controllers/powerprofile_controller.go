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
	"time"

	powerv1 "github.com/intel/kubernetes-power-manager/api/v1"
	"github.com/intel/power-optimization-library/pkg/power"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// performance          ===>  priority level 0
// balance_performance  ===>  priority level 1
// balance_power        ===>  priority level 2
// power                ===>  priority level 3

// PowerProfileReconciler reconciles a PowerProfile object
type PowerProfileReconciler struct {
	client.Client
	Log          logr.Logger
	Scheme       *runtime.Scheme
	PowerLibrary power.Host
}

// +kubebuilder:rbac:groups=power.intel.com,resources=powerprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=power.intel.com,resources=powerprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

// Reconcile method that implements the reconcile loop
func (r *PowerProfileReconciler) Reconcile(c context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("powerprofile", req.NamespacedName)
	logger.Info("Reconciling the power profile")

	var err error
	if req.Namespace != IntelPowerNamespace {
		err := fmt.Errorf("incorrect namespace")
		logger.Error(err, "resource is not in the intel-power namespace, ignoring")
		return ctrl.Result{Requeue: false}, err
	}

	// Node name is passed down via the downwards API and used to make sure the PowerProfile is for this node
	nodeName := os.Getenv("NODE_NAME")

	profile := &powerv1.PowerProfile{}
	defer func() { _ = writeUpdatedStatusErrsIfRequired(c, r.Status(), profile, err) }()
	err = r.Client.Get(context.TODO(), req.NamespacedName, profile)
	logger.V(5).Info("retrieving the power profile instances")
	if err != nil {
		if errors.IsNotFound(err) {
			// First we need to remove the profile from the Power library, this will in turn remove the pool,
			// which will also move the cores back to the Shared/Default pool and reconfigure them. We then
			// need to remove the Power Workload from the cluster, which in this case will do nothing as
			// everything has already been removed. Finally, we remove the Extended Resources from the Node
			// first we make sure the profile isn't the one used by the shared pool
			if r.PowerLibrary.GetSharedPool().GetPowerProfile() != nil && req.Name == r.PowerLibrary.GetSharedPool().GetPowerProfile().Name() {
				err := r.PowerLibrary.GetSharedPool().SetPowerProfile(nil)
				if err != nil {
					logger.Error(err, "error setting nil profile")
					return ctrl.Result{}, err
				}
				pool := r.PowerLibrary.GetExclusivePool(req.Name)
				if pool == nil {
					notFoundErr := fmt.Errorf("pool not found")
					logger.Error(notFoundErr, fmt.Sprintf("attempted to remove the non existing pool %s", req.Name))
					return ctrl.Result{Requeue: false}, notFoundErr
				}
				err = pool.Remove()
				if err != nil {
					logger.Error(err, "error deleting the power profile from the library")
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}

			powerWorkloadName := fmt.Sprintf("%s-%s", req.NamespacedName.Name, nodeName)
			powerWorkload := &powerv1.PowerWorkload{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      powerWorkloadName,
				Namespace: req.NamespacedName.Namespace,
			}, powerWorkload)
			if err != nil {
				if !errors.IsNotFound(err) {
					logger.Error(err, fmt.Sprintf("error deleting the power workload '%s' from the cluster", powerWorkloadName))
					return ctrl.Result{}, err
				}
			} else {
				err = r.Client.Delete(context.TODO(), powerWorkload)
				if err != nil {
					logger.Error(err, fmt.Sprintf("error deleting the power workload '%s' from the cluster", powerWorkloadName))
					return ctrl.Result{}, err
				}
			}
			// Remove the extended resources for this power profile from the node.
			err = r.removeExtendedResources(nodeName, req.NamespacedName.Name, &logger)
			if err != nil {
				logger.Error(err, "error removing the extended resources from the node")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		// Requeue the request.
		return ctrl.Result{}, err
	}

	// Check if this profile should be applied to this node. The check applies to both shared and non-shared profiles.
	match, err := doesNodeMatchPowerProfileSelector(r.Client, profile, nodeName, &logger)
	if err != nil {
		logger.Error(err, "error checking if node matches power profile selector")
		return ctrl.Result{}, err
	}
	if !match {
		logger.V(5).Info("Profile not applicable to this node due to node selector", "nodeName", nodeName, "nodeSelector", profile.Spec.NodeSelector)

		// Clean up resources if they exist on this node but shouldn't anymore.
		err = r.cleanupProfileFromNode(profile, nodeName, &logger)
		if err != nil {
			logger.Error(err, "error cleaning up profile resources from node")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Make sure the EPP value is one of the correct ones or empty in the case of a user-created profile.
	logger.V(5).Info("confirming EPP value is one of the correct values")
	if profile.Spec.PStates.Epp != "" {
		isValid := isValidEpp(profile.Spec.PStates.Epp)

		if !isValid {
			incorrectEppErr := errors.NewServiceUnavailable(fmt.Sprintf("EPP value not allowed: %v - deleting the power profile CRD", profile.Spec.PStates.Epp))
			logger.Error(incorrectEppErr, "error reconciling the power profile")

			err = r.Client.Delete(context.TODO(), profile)
			if err != nil {
				logger.Error(err, fmt.Sprintf("error deleting the power profile %s with the incorrect EPP value %s", profile.Spec.Name, profile.Spec.PStates.Epp))
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
	}

	// Validate the EPP value.
	actualEpp := profile.Spec.PStates.Epp
	if !power.IsFeatureSupported(power.EPPFeature) && actualEpp != "" {
		err = fmt.Errorf("EPP is not supported but %s provides one, setting EPP to ''", profile.Name)
		logger.Error(err, "invalid EPP")
		actualEpp = ""
	}

	// Create and validate power profile in the power library
	powerProfile, err := power.NewPowerProfile(
		profile.Spec.Name, profile.Spec.PStates.Min, profile.Spec.PStates.Max,
		profile.Spec.PStates.Governor, actualEpp,
		profile.Spec.CStates.Names, profile.Spec.CStates.MaxLatencyUs)
	if err != nil {
		logger.Error(err, "could not create the power profile")
		return ctrl.Result{Requeue: false}, err
	}
	// An exclusive pool should be created for both shared and non-shared profiles.
	profileFromLibrary := r.PowerLibrary.GetExclusivePool(profile.Spec.Name)
	if profileFromLibrary == nil {
		pool, err := r.PowerLibrary.AddExclusivePool(profile.Spec.Name)
		if err != nil {
			logger.Error(err, "failed to create the power profile")
			return ctrl.Result{}, err
		}
		err = pool.SetPowerProfile(powerProfile)
		if err != nil {
			logger.Error(err, fmt.Sprintf("error adding the profile '%s' to the power library for host '%s'", profile.Spec.Name, nodeName))
			return ctrl.Result{}, err
		}

		// This block is trying to handle the edge case where a shared workload references a profile that was created later,
		// but this approach is incomplete and problematic because:
		//      1. Shared workloads can reference non-shared profiles
		//      2. Shared workload naming is not guaranteed to follow the shared-<NodeName>-workload format
		// TODO: Remove this workaround and implement proper webhook validation to reject workload creation if referenced profile is not found
		if profile.Spec.Shared {
			logger.V(5).Info(fmt.Sprintf(
				"shared power profile successfully created: name - %s max - %d min - %d EPP - %s",
				profile.Spec.Name, powerProfile.GetPStates().GetMaxFreq().IntVal, powerProfile.GetPStates().GetMinFreq().IntVal, actualEpp))
			workloadName := fmt.Sprintf("shared-%s-workload", nodeName)
			// check if the current node has a shared workload
			workload := &powerv1.PowerWorkload{}
			err = r.Client.Get(context.TODO(), client.ObjectKey{
				Name:      workloadName,
				Namespace: req.NamespacedName.Namespace,
			}, workload)
			if err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				logger.Error(err, "client error")
				return ctrl.Result{}, err
			}
			// check if the shared workload uses this profile
			if workload.Spec.PowerProfile == profile.Spec.Name {
				// if workload uses this profile, update it to use the latest info
				workload.ObjectMeta.Annotations = map[string]string{"PM-updated": fmt.Sprint(time.Now().Unix())}
				err = r.Client.Update(context.TODO(), workload)
				if err != nil {
					logger.Error(err, "error updating workload attached to profile")
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}

		logger.V(5).Info(fmt.Sprintf(
			"power profile successfully created: name - %s max - %d min - %d EPP - %s",
			profile.Spec.Name, powerProfile.GetPStates().GetMaxFreq().IntVal, powerProfile.GetPStates().GetMinFreq().IntVal, actualEpp))
	} else {
		// Exclusive pool for this profile already exists, update it and all the other pools that use this profile
		err = r.PowerLibrary.GetExclusivePool(profile.Spec.Name).SetPowerProfile(powerProfile)
		msg := fmt.Sprintf("updating the power profile '%s' to the power library for node '%s'", profile.Spec.Name, nodeName)
		logger.V(5).Info(msg)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("error %s: %v", msg, err)
		}

		// Update shared pool if it uses this profile
		sharedPool := r.PowerLibrary.GetSharedPool()
		if sharedPool.GetPowerProfile() != nil && sharedPool.GetPowerProfile().Name() == profile.Spec.Name {
			msg := fmt.Sprintf("updating shared pool in power library with updated profile '%s' for node '%s'", profile.Spec.Name, nodeName)
			logger.V(5).Info(msg)
			err := sharedPool.SetPowerProfile(powerProfile)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("error %s: %v", msg, err)
			}
		}

		// Update any special reserved pools created for reservedCPUs that use this profile
		exclusivePools := r.PowerLibrary.GetAllExclusivePools()
		for _, pool := range *exclusivePools {
			if strings.Contains(pool.Name(), nodeName+"-reserved-") &&
				pool.GetPowerProfile() != nil &&
				pool.GetPowerProfile().Name() == profile.Spec.Name {
				msg := fmt.Sprintf("updating special reserved pool '%s' in power library with updated profile '%s' for node '%s'", pool.Name(), profile.Spec.Name, nodeName)
				logger.V(5).Info(msg)
				err := pool.SetPowerProfile(powerProfile)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("error %s: %v", msg, err)
				}
			}
		}

		logger.V(5).Info(fmt.Sprintf(
			"power profile successfully updated: name - %s max - %d min - %d EPP - %s",
			profile.Spec.Name, powerProfile.GetPStates().GetMaxFreq().IntVal, powerProfile.GetPStates().GetMinFreq().IntVal, actualEpp))
	}

	if profile.Spec.Shared {
		// Return for shared profiles, as extended resources and workloads are not created for them
		return ctrl.Result{}, nil
	}

	// Create or update the extended resources for the profile.
	err = r.ensureExtendedResources(nodeName, profile, &logger)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error creating or updating the extended resources for the base profile: %v", err)
	}

	// Create the power workload for the profile
	workloadName := fmt.Sprintf("%s-%s", profile.Spec.Name, nodeName)
	logger.V(5).Info(fmt.Sprintf("configuring the workload name: %s", workloadName))
	workload := &powerv1.PowerWorkload{}
	err = r.Client.Get(context.TODO(), client.ObjectKey{
		Name:      workloadName,
		Namespace: req.NamespacedName.Namespace,
	}, workload)
	if err != nil {
		if errors.IsNotFound(err) {
			powerWorkloadSpec := &powerv1.PowerWorkloadSpec{
				Name:         workloadName,
				PowerProfile: profile.Spec.Name,
			}

			powerWorkload := &powerv1.PowerWorkload{
				ObjectMeta: metav1.ObjectMeta{
					Name:      workloadName,
					Namespace: req.NamespacedName.Namespace,
				},
			}
			powerWorkload.Spec = *powerWorkloadSpec

			err = r.Client.Create(context.TODO(), powerWorkload)
			if err != nil {
				logger.Error(err, fmt.Sprintf("error creating the power workload '%s'", workloadName))
				return ctrl.Result{}, err
			}

			logger.V(5).Info("power workload successfully created", "name", workloadName, "profile", profile.Spec.Name)
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	// If the workload already exists then the power profile was just updated and the power library will take care of reconfiguring cores
	return ctrl.Result{}, nil
}

func (r *PowerProfileReconciler) ensureExtendedResources(nodeName string, profile *powerv1.PowerProfile, logger *logr.Logger) error {
	node := &corev1.Node{}
	err := r.Client.Get(context.TODO(), client.ObjectKey{
		Name: nodeName,
	}, node)
	if err != nil {
		return err
	}

	totalCPUs := len(*r.PowerLibrary.GetAllCpus())
	logger.V(0).Info("configuring based on the capacity associated to the specific power profile")

	// Calculate CPU count based on profile's CpuCapacity or default to all CPUs.
	var numExtendedResources int64
	if profile.Spec.CpuCapacity.String() != "" {
		// Use the standard library function to handle IntOrString properly.
		absoluteCPUs, err := intstr.GetScaledValueFromIntOrPercent(&profile.Spec.CpuCapacity, totalCPUs, false)
		if err == nil && absoluteCPUs > 0 {
			numExtendedResources = int64(absoluteCPUs)
		} else {
			// Fallback to all CPUs if parsing fails
			logger.Error(err, "could not parse power profile cpu capacity, using total CPUs", "error", err, "absoluteCPUs", absoluteCPUs)
			numExtendedResources = int64(totalCPUs)
		}
	} else {
		// Default to all CPUs if no configuration found.
		numExtendedResources = int64(totalCPUs)
	}

	profilesAvailable := resource.NewQuantity(numExtendedResources, resource.DecimalSI)
	extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, profile.Spec.Name))
	node.Status.Capacity[extendedResourceName] = *profilesAvailable

	err = r.Client.Status().Update(context.TODO(), node)
	if err != nil {
		return err
	}

	return nil
}

func (r *PowerProfileReconciler) removeExtendedResources(nodeName string, profileName string, logger *logr.Logger) error {
	node := &corev1.Node{}
	err := r.Client.Get(context.TODO(), client.ObjectKey{
		Name: nodeName,
	}, node)
	if err != nil {
		return err
	}

	logger.V(5).Info("removing the extended resources")
	newNodeCapacityList := make(map[corev1.ResourceName]resource.Quantity)
	extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, profileName))
	for resourceFromNode, numberOfResources := range node.Status.Capacity {
		if resourceFromNode == extendedResourceName {
			continue
		}
		newNodeCapacityList[resourceFromNode] = numberOfResources
	}

	node.Status.Capacity = newNodeCapacityList
	err = r.Client.Status().Update(context.TODO(), node)
	if err != nil {
		return err
	}

	return nil
}

// cleanupProfileFromNode removes only extended resources from a node when it no longer matches the PowerProfile selector.
func (r *PowerProfileReconciler) cleanupProfileFromNode(profile *powerv1.PowerProfile, nodeName string, logger *logr.Logger) error {
	logger.V(5).Info("Cleaning up PowerProfile extended resources from node", "profile", profile.Spec.Name, "nodeName", nodeName)

	// Only remove extended resources from the node.
	// Keep pools, workloads, and shared PowerProfile configurations as pods/services may depend on them.
	err := r.removeExtendedResources(nodeName, profile.Spec.Name, logger)
	if err != nil {
		logger.Error(err, "error removing extended resources")
		return err
	}

	logger.V(5).Info("Successfully cleaned up PowerProfile extended resources from node", "profile", profile.Spec.Name, "nodeName", nodeName)
	return nil
}

// SetupWithManager specifies how the controller is built and watch a CR and other resources that are owned and managed by the controller
func (r *PowerProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&powerv1.PowerProfile{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToProfileRequests),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					// Filter out the nodes that are not the current node.
					if e.ObjectOld.GetName() != os.Getenv("NODE_NAME") {
						return false
					}
					// Filter for label updates only.
					return !equality.Semantic.DeepEqual(e.ObjectNew.GetLabels(), e.ObjectOld.GetLabels())
				},
				CreateFunc:  func(e event.CreateEvent) bool { return true },
				GenericFunc: func(ge event.GenericEvent) bool { return false },
				DeleteFunc: func(de event.DeleteEvent) bool {
					// Filter the current node.
					return de.Object.GetName() == os.Getenv("NODE_NAME")
				},
			})).
		Complete(r)
}

// nodeToProfileRequests maps Node events to PowerProfile reconciliation requests
func (r *PowerProfileReconciler) nodeToProfileRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	node := obj.(*corev1.Node)
	r.Log.V(5).Info("Node change detected, checking PowerProfiles", "nodeName", node.Name)

	var requests []reconcile.Request

	// List all PowerProfiles.
	powerProfiles := &powerv1.PowerProfileList{}
	if err := r.Client.List(ctx, powerProfiles); err != nil {
		r.Log.Error(err, "Failed to list PowerProfiles for node event handling")
		return requests
	}

	// Enqueue reconciliation for all PowerProfiles that might be affected.
	for _, profile := range powerProfiles.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      profile.Name,
				Namespace: profile.Namespace,
			},
		})
	}

	r.Log.V(5).Info("Enqueuing PowerProfile reconciliation requests", "count", len(requests), "nodeName", node.Name)
	return requests
}
