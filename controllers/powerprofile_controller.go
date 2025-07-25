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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// value deducted from the max freq of a default profile
// this gives the profile a 0.2 Ghz frequency range
const MinFreqOffset = 200

// performance          ===>  priority level 0
// balance_performance  ===>  priority level 1
// balance_power        ===>  priority level 2
// power                ===>  priority level 3

var profilePercentages map[string]map[string]float64 = map[string]map[string]float64{
	"performance": {
		"resource":   .40,
		"difference": 0.0,
	},
	"balance_performance": {
		"resource":   .60,
		"difference": .25,
	},
	"balance_power": {
		"resource":   .80,
		"difference": .50,
	},
	"power": {
		"resource":   1.0,
		"difference": 0.0,
	},
	// We have the empty string here so users can create power profiles that are not associated with SST-CP
	"": {
		"resource": 1.0,
	},
}

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
	var err error
	logger := r.Log.WithValues("powerprofile", req.NamespacedName)
	if req.Namespace != IntelPowerNamespace {
		err := fmt.Errorf("incorrect namespace")
		logger.Error(err, "resource is not in the intel-power namespace, ignoring")
		return ctrl.Result{Requeue: false}, err
	}
	logger.Info("reconciling the power profile")

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
			// Remove the extended resources for this power profile from the node
			err = r.removeExtendedResources(nodeName, req.NamespacedName.Name, &logger)
			if err != nil {
				logger.Error(err, "error removing the extended resources from the node")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		// Requeue the request
		return ctrl.Result{}, err
	}

	// Make sure the EPP value is one of the four correct ones or empty in the case of a user-created profile
	logger.V(5).Info("confirming EPP value is one of the correct values")
	if _, exists := profilePercentages[profile.Spec.PStates.Epp]; !exists {
		incorrectEppErr := errors.NewServiceUnavailable(fmt.Sprintf("EPP value not allowed: %v - deleting the power profile CRD", profile.Spec.PStates.Epp))
		logger.Error(incorrectEppErr, "error reconciling the power profile")

		err = r.Client.Delete(context.TODO(), profile)
		if err != nil {
			logger.Error(err, fmt.Sprintf("error deleting the power profile %s with the incorrect EPP value %s", profile.Spec.Name, profile.Spec.PStates.Epp))
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	absoluteMinimumFrequency, absoluteMaximumFrequency, err := getMaxMinFrequencyValues(r.PowerLibrary)
	logger.V(5).Info("retrieving the max possible frequency and min possible frequency from the system")
	if err != nil {
		logger.Error(err, "error retrieving the frequency values from the node")
		return ctrl.Result{Requeue: false}, err
	}

	var profileMaxFreq int
	var profileMinFreq int
	// Use default hardware limits if not specified
	if profile.Spec.PStates.Max == 0 {
		profileMaxFreq = absoluteMaximumFrequency
	} else {
		profileMaxFreq = profile.Spec.PStates.Max
	}
	if profile.Spec.PStates.Min == 0 {
		profileMinFreq = absoluteMinimumFrequency
	} else {
		profileMinFreq = profile.Spec.PStates.Min
	}

	// If both max and min are not specified and it's EPP-based profile, override with EPP-based calculation
	if profile.Spec.PStates.Epp != "" && profile.Spec.PStates.Max == 0 && profile.Spec.PStates.Min == 0 {
		profileMaxFreq = int(float64(absoluteMaximumFrequency) - (float64((absoluteMaximumFrequency - absoluteMinimumFrequency)) * profilePercentages[profile.Spec.PStates.Epp]["difference"]))
		profileMinFreq = int(profileMaxFreq) - MinFreqOffset
	}

	if profileMaxFreq > absoluteMaximumFrequency || profileMinFreq < absoluteMinimumFrequency {
		err = fmt.Errorf("max and min frequency must be within the range %d-%d", absoluteMinimumFrequency, absoluteMaximumFrequency)
		logger.Error(err, fmt.Sprintf("error creating the profile '%s'", profile.Spec.Name))
		return ctrl.Result{Requeue: false}, err
	}
	actualEpp := profile.Spec.PStates.Epp
	if !power.IsFeatureSupported(power.EPPFeature) && actualEpp != "" {
		err = fmt.Errorf("EPP is not supported but %s provides one, setting EPP to ''", profile.Name)
		logger.Error(err, "invalid EPP")
		actualEpp = ""
	}

	// Create and validate power profile in the power library
	powerProfile, err := power.NewPowerProfile(
		profile.Spec.Name, uint(profileMinFreq), uint(profileMaxFreq),
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
				profile.Spec.Name, profileMaxFreq, profileMinFreq, actualEpp))
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
			profile.Spec.Name, profileMaxFreq, profileMinFreq, actualEpp))
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
			profile.Spec.Name, profileMaxFreq, profileMinFreq, actualEpp))
	}

	if profile.Spec.Shared {
		// Return for shared profiles, as extended resources and workloads are not created for them
		return ctrl.Result{}, nil
	}

	// Create or update the extended resources for the profile
	err = r.ensureExtendedResources(nodeName, profile.Spec.Name, profile.Spec.PStates.Epp, &logger)
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

func (r *PowerProfileReconciler) ensureExtendedResources(nodeName string, profileName string, eppValue string, logger *logr.Logger) error {
	node := &corev1.Node{}
	err := r.Client.Get(context.TODO(), client.ObjectKey{
		Name: nodeName,
	}, node)
	if err != nil {
		return err
	}

	numCPUsOnNode := float64(len(*r.PowerLibrary.GetAllCpus()))
	logger.V(5).Info("configuring based on the percentage associated to the specific power profile")
	numExtendedResources := int64(numCPUsOnNode * profilePercentages[eppValue]["resource"])
	profilesAvailable := resource.NewQuantity(numExtendedResources, resource.DecimalSI)
	extendedResourceName := corev1.ResourceName(fmt.Sprintf("%s%s", ExtendedResourcePrefix, profileName))
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

func getMaxMinFrequencyValues(h power.Host) (int, int, error) {
	typeList := h.GetFreqRanges()
	if len(typeList) == 0 {
		return 0, 0, fmt.Errorf("could not retireve hardware frequency limits")
	}
	absoluteMaximumFrequency := int(typeList[0].GetMax())
	absoluteMinimumFrequency := int(typeList[0].GetMin())

	absoluteMaximumFrequency = absoluteMaximumFrequency / 1000
	absoluteMinimumFrequency = absoluteMinimumFrequency / 1000

	return absoluteMinimumFrequency, absoluteMaximumFrequency, nil
}

// SetupWithManager specifies how the controller is built and watch a CR and other resources that are owned and managed by the controller
func (r *PowerProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&powerv1.PowerProfile{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
