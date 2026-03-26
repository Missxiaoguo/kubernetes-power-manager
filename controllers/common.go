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
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const queuetime = time.Second * 5

// FieldOwnerPowerProfileController is the base field manager name for PowerProfile controller.
const FieldOwnerPowerProfileController = "powerprofile-controller"

// powerProfileFieldManager returns the field manager name for a specific PowerProfile.
// Using per-profile field managers enables SSA to track ownership at the element level
// for the map-type PowerProfiles list (with listMapKey=name).
func powerProfileFieldManager(profileName string) string {
	return fmt.Sprintf("%s.%s", FieldOwnerPowerProfileController, profileName)
}

// ValidEppValues defines the valid EPP (Energy Performance Preference) values
var ValidEppValues = []string{"performance", "balance_performance", "balance_power", "power"}

// write errors to the status filed, pass nil to clear errors, will only do update resource is valid and not being deleted
// if object already has the correct errors it will not be updated in the API
func writeUpdatedStatusErrsIfRequired(ctx context.Context, statusWriter client.SubResourceWriter, object powerv1.PowerCRWithStatusErrors, objectErrors error) error {
	var err error
	// if invalid or marked for deletion don't do anything
	if object.GetUID() == "" || object.GetDeletionTimestamp() != nil {
		return err
	}
	errList := util.UnpackErrsToStrings(objectErrors)
	// no updates are needed
	if equality.Semantic.DeepEqual(*errList, *object.GetStatusErrors()) {
		return err
	}

	// Use Patch for safer status update across multiple agents
	orig, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object does not implement client.Object")
	}
	statusPatch := client.MergeFrom(orig)

	object.SetStatusErrors(errList)
	err = statusWriter.Patch(ctx, object, statusPatch)
	if err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "failed to write status update")
	}
	return err
}

// isValidEpp checks if a certain name corresponds to a valid EPP value.
func isValidEpp(inputName string) bool {
	for _, validEpp := range ValidEppValues {
		if inputName == validEpp {
			return true
		}
	}
	return false
}

// doesNodeMatchPowerProfileSelector checks if a PowerProfile should be applied to a specific node.
func doesNodeMatchPowerProfileSelector(c client.Client, profile *powerv1.PowerProfile, nodeName string, logger *logr.Logger) (bool, error) {
	logger.V(5).Info("Checking if PowerProfile should be applied to node", "profile", profile.Spec.Name, "nodeName", nodeName)
	// If no label selector is specified, apply to all nodes.
	labelSelector := profile.Spec.NodeSelector.LabelSelector
	if len(labelSelector.MatchLabels) == 0 && len(labelSelector.MatchExpressions) == 0 {
		logger.V(5).Info("No label selector specified, applying PowerProfile to all nodes", "profile", profile.Spec.Name, "nodeName", nodeName)
		return true, nil
	}

	// Get the node to check its labels.
	node := &corev1.Node{}
	err := c.Get(context.TODO(), client.ObjectKey{Name: nodeName}, node)
	if err != nil {
		// If we can't get the node, don't apply the profile.
		logger.Error(err, "Failed to get node for selector validation", "nodeName", nodeName)
		return false, err
	}

	// Convert the label selector to a Selector and check if it matches the node.
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		logger.Error(err, "Failed to convert label selector", "selector", labelSelector)
		return false, err
	}

	// Check if the node's labels match the selector.
	res := selector.Matches(labels.Set(node.Labels))
	logger.V(5).Info("Node label check result", "nodeName", nodeName, "selector", selector, "nodeLabels", node.Labels, "result", res)
	return res, nil
}

// validateProfileAvailabilityOnNode validates that a PowerProfile exists in the cluster and is available to be used on the node
func validateProfileAvailabilityOnNode(ctx context.Context, c client.Client, profileName string, nodeName string, powerLibrary power.Host, logger *logr.Logger) (bool, error) {
	if profileName == "" {
		return true, nil
	}

	powerProfile := &powerv1.PowerProfile{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      profileName,
		Namespace: PowerNamespace,
	}, powerProfile)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if powerLibrary == nil {
		return false, fmt.Errorf("power library is nil")
	}

	// PowerProfile exists in the cluster and pool exists indicates profile is available to be used on the node.
	pool := powerLibrary.GetExclusivePool(profileName)
	if pool == nil {
		logger.Error(fmt.Errorf("pool '%s' not found", profileName), "pool not found")
		return false, nil
	}

	// Verify the node matches the PowerProfile node selector.
	match, err := doesNodeMatchPowerProfileSelector(c, powerProfile, nodeName, logger)
	if err != nil {
		logger.Error(err, "error checking if node matches power profile selector")
		return false, err
	}
	return match, nil
}

// formatIntOrString formats an IntOrString pointer as a string.
// Returns empty string if nil, the integer value as string for Int type,
// or the string value for String type.
func formatIntOrString(value *intstr.IntOrString) string {
	if value == nil {
		return ""
	}
	if value.Type == intstr.Int {
		return strconv.Itoa(int(value.IntVal))
	}
	return value.StrVal
}

// applyPowerNodeStateProfilesStatus applies the given PowerProfiles to the PowerNodeState status
// using Server-Side Apply. The fieldManager parameter enables per-profile ownership — each profile
// should use a unique field manager (e.g., "powerprofile-controller.profile-name") so that SSA can
// track ownership at the element level for the map-type PowerProfiles list.
func applyPowerNodeStateProfilesStatus(ctx context.Context, c client.Client, powerNodeStateName string, profiles []powerv1.PowerNodeProfileStatus, fieldManager string) error {
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
			PowerProfiles: profiles,
		},
	}

	return c.Status().Patch(ctx, patchNodeState, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
}

// addPowerNodeStatusProfileEntry updates the PowerNodeState CR for a given node with the validation
// results of a PowerProfile using Server-Side Apply (SSA).
//
// This function uses per-profile field managers (e.g., "powerprofile-controller.profile-name") to enable
// concurrent updates from different profile controllers. Since PowerProfiles is a map-type list keyed by
// name, SSA merges entries by key - each controller only owns its own profile entry.
func addPowerNodeStatusProfileEntry(ctx context.Context, c client.Client, nodeName string, profile *powerv1.PowerProfile, profileErrors error, logger *logr.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName cannot be empty")
	}

	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)

	// Serialize the PowerProfile spec to a readable config string.
	cstatesString := prettifyCStatesMap(profile.Spec.CStates.Names)
	if cstatesString == "" && profile.Spec.CStates.MaxLatencyUs != nil {
		cstatesString = fmt.Sprintf("maxLatency: %d", *profile.Spec.CStates.MaxLatencyUs)
	}
	config := fmt.Sprintf(
		"Min: %s, Max: %s, Governor: %s, EPP: %s, C-States: %s",
		formatIntOrString(profile.Spec.PStates.Min), formatIntOrString(profile.Spec.PStates.Max),
		profile.Spec.PStates.Governor, profile.Spec.PStates.Epp, cstatesString)

	errList := util.UnpackErrsToStrings(profileErrors)
	profileStatus := powerv1.PowerNodeProfileStatus{Name: profile.Spec.Name, Config: config, Errors: *errList}
	fieldManager := powerProfileFieldManager(profile.Spec.Name)

	err := applyPowerNodeStateProfilesStatus(ctx, c, powerNodeStateName, []powerv1.PowerNodeProfileStatus{profileStatus}, fieldManager)
	if err != nil {
		if errors.IsNotFound(err) {
			// Profile was already created, but status wasn't recorded.
			// Return error to requeue so we retry once PowerConfig creates the CR.
			logger.Info("PowerNodeState not found, requeueing to record profile status", "powerNodeState", powerNodeStateName)
			return err
		}
		return err
	}

	logger.Info("Updated PowerNodeState with profile validation results",
		"powerNodeState", powerNodeStateName,
		"profile", profile.Spec.Name,
		"errors", len(*errList))

	return nil
}

// removePowerNodeStatusProfileEntry removes a PowerProfile entry from the PowerNodeState status.
//
// This function uses per-profile field managers to release ownership of the profile entry.
// Applying an empty PowerProfiles list causes SSA to prune the entry this manager previously
// owned, while preserving entries owned by other field managers.
func removePowerNodeStatusProfileEntry(ctx context.Context, c client.Client, nodeName string, profileName string, logger *logr.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName cannot be empty")
	}

	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)
	fieldManager := powerProfileFieldManager(profileName)

	if err := applyPowerNodeStateProfilesStatus(ctx, c, powerNodeStateName, []powerv1.PowerNodeProfileStatus{}, fieldManager); err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("PowerNodeState not found, nothing to remove", "powerNodeState", powerNodeStateName)
			return nil
		}
		return err
	}

	logger.Info("Removed profile from PowerNodeState",
		"powerNodeState", powerNodeStateName,
		"profile", profileName)

	return nil
}
