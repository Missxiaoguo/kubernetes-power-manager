//go:build envtest

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
	"testing"

	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSSA_ProfileFieldManagerOwnership(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	pnsName := "test-node-power-state"
	createTestPowerNodeState(t, cl, pnsName)

	// Apply two profiles with different field managers.
	profiles1 := []powerv1.PowerNodeProfileStatus{
		{Name: "performance", Config: "Min: 3200, Max: 3600"},
	}
	profiles2 := []powerv1.PowerNodeProfileStatus{
		{Name: "balance-power", Config: "Min: 800, Max: 2000"},
	}

	fm1 := powerProfileFieldManager("performance")
	fm2 := powerProfileFieldManager("balance-power")

	// Verify field manager format.
	assert.Equal(t, "powerprofile-controller.performance", fm1)
	assert.Equal(t, "powerprofile-controller.balance-power", fm2)

	// Apply profile 1.
	err := applyPowerNodeStateProfilesStatus(ctx, cl, pnsName, profiles1, fm1)
	require.NoError(t, err)

	// Apply profile 2.
	err = applyPowerNodeStateProfilesStatus(ctx, cl, pnsName, profiles2, fm2)
	require.NoError(t, err)

	// Both profiles should coexist.
	pns := &powerv1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	assert.Len(t, pns.Status.PowerProfiles, 2, "both profiles should be present")
	profileNames := []string{pns.Status.PowerProfiles[0].Name, pns.Status.PowerProfiles[1].Name}
	assert.Contains(t, profileNames, "performance")
	assert.Contains(t, profileNames, "balance-power")
}

func TestSSA_ProfileRemovalPreservesOtherProfiles(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	pnsName := "test-node-power-state"
	createTestPowerNodeState(t, cl, pnsName)

	// Apply two profiles.
	fm1 := powerProfileFieldManager("performance")
	fm2 := powerProfileFieldManager("balance-power")

	err := applyPowerNodeStateProfilesStatus(ctx, cl, pnsName,
		[]powerv1.PowerNodeProfileStatus{{Name: "performance", Config: "config1"}}, fm1)
	require.NoError(t, err)

	err = applyPowerNodeStateProfilesStatus(ctx, cl, pnsName,
		[]powerv1.PowerNodeProfileStatus{{Name: "balance-power", Config: "config2"}}, fm2)
	require.NoError(t, err)

	// Verify both profiles are present.
	pns := &powerv1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)
	assert.Len(t, pns.Status.PowerProfiles, 2, "both profiles should be present")
	assert.Equal(t, "performance", pns.Status.PowerProfiles[0].Name)
	assert.Equal(t, "balance-power", pns.Status.PowerProfiles[1].Name)

	// Remove profile 1 by applying empty list with its field manager.
	err = applyPowerNodeStateProfilesStatus(ctx, cl, pnsName,
		[]powerv1.PowerNodeProfileStatus{}, fm1)
	require.NoError(t, err)

	// Only profile 2 should remain.
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	assert.Len(t, pns.Status.PowerProfiles, 1, "only balance-power should remain")
	assert.Equal(t, "balance-power", pns.Status.PowerProfiles[0].Name)
}

func TestSSA_ProfileUpdatePreservesEntry(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	pnsName := "test-node-power-state"
	createTestPowerNodeState(t, cl, pnsName)

	fm := powerProfileFieldManager("performance")

	// Apply initial profile.
	err := applyPowerNodeStateProfilesStatus(ctx, cl, pnsName,
		[]powerv1.PowerNodeProfileStatus{{Name: "performance", Config: "config1"}}, fm)
	require.NoError(t, err)

	// Update the same profile with new config and errors.
	err = applyPowerNodeStateProfilesStatus(ctx, cl, pnsName,
		[]powerv1.PowerNodeProfileStatus{{Name: "performance", Config: "config2", Errors: []string{"epp invalid"}}}, fm)
	require.NoError(t, err)

	pns := &powerv1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	assert.Len(t, pns.Status.PowerProfiles, 1)
	assert.Equal(t, "config2", pns.Status.PowerProfiles[0].Config)
	assert.Equal(t, []string{"epp invalid"}, pns.Status.PowerProfiles[0].Errors)
}
