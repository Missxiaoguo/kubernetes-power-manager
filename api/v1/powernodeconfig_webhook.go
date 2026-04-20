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

package v1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-power-openshift-io-v1-powernodeconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=power.openshift.io,resources=powernodeconfigs,verbs=create;update,versions=v1,name=vpowernodeconfig.kb.io,admissionReviewVersions=v1

var powernodeconfiglog = logf.Log.WithName("powernodeconfig-webhook")

// powerNodeConfigValidator implements admission.CustomValidator for PowerNodeConfig.
type powerNodeConfigValidator struct{}

// SetupPowerNodeConfigWebhookWithManager registers the validating webhook for PowerNodeConfig.
func SetupPowerNodeConfigWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&PowerNodeConfig{}).
		WithValidator(&powerNodeConfigValidator{}).
		Complete()
}

var _ webhook.CustomValidator = &powerNodeConfigValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *powerNodeConfigValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	config, ok := obj.(*PowerNodeConfig)
	if !ok {
		return nil, fmt.Errorf("expected PowerNodeConfig, got %T", obj)
	}
	powernodeconfiglog.Info("validating create", "name", config.Name)
	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator.
func (v *powerNodeConfigValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	config, ok := newObj.(*PowerNodeConfig)
	if !ok {
		return nil, fmt.Errorf("expected PowerNodeConfig, got %T", newObj)
	}
	powernodeconfiglog.Info("validating update", "name", config.Name)
	return nil, nil
}

// ValidateDelete implements admission.CustomValidator.
func (v *powerNodeConfigValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
