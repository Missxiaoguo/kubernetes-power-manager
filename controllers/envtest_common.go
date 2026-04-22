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
	"path/filepath"
	"testing"

	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// setupEnvTest starts an envtest API server and returns a client and cleanup function.
func setupEnvTest(t *testing.T) (client.Client, func()) {
	t.Helper()

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
		},
	}

	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to start envtest")
	require.NotNil(t, cfg)

	err = powerv1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	// Create the test namespace.
	ns := &metav1.ObjectMeta{Name: PowerNamespace}
	nsObj := &corev1.Namespace{ObjectMeta: *ns}
	_ = cl.Create(context.TODO(), nsObj)

	return cl, func() {
		err := testEnv.Stop()
		if err != nil {
			t.Logf("failed to stop envtest: %v", err)
		}
	}
}

// createTestPowerNodeState creates a PowerNodeState for testing.
func createTestPowerNodeState(t *testing.T, cl client.Client, name string) {
	t.Helper()
	pns := &powerv1.PowerNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
	}
	err := cl.Create(context.TODO(), pns)
	require.NoError(t, err)
}

// newTestPowerProfile creates a PowerProfile for testing.
func newTestPowerProfile(name string, shared bool) *powerv1.PowerProfile {
	return &powerv1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PowerNamespace},
		Spec:       powerv1.PowerProfileSpec{Shared: shared},
	}
}

// createTestNode creates a Node with the given labels.
func createTestNode(t *testing.T, cl client.Client, name string, labels map[string]string) {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	require.NoError(t, cl.Create(context.TODO(), node))
}

// createTestPowerNodeConfig creates a PowerNodeConfig with the given spec.
func createTestPowerNodeConfig(t *testing.T, cl client.Client, name string, sharedProfile string, matchLabels map[string]string, reserved []powerv1.ReservedSpec) {
	t.Helper()
	config := &powerv1.PowerNodeConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: PowerNamespace,
		},
		Spec: powerv1.PowerNodeConfigSpec{
			SharedPowerProfile: sharedProfile,
			ReservedCPUs:       reserved,
		},
	}
	if matchLabels != nil {
		config.Spec.NodeSelector = powerv1.NodeSelector{
			LabelSelector: metav1.LabelSelector{MatchLabels: matchLabels},
		}
	}
	require.NoError(t, cl.Create(context.TODO(), config))
}
