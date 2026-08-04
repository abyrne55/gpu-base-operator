/*
Copyright 2026 Intel Corporation. All Rights Reserved.

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

package controller

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
)

// +kubebuilder:rbac:groups=kmm.sigs.x-k8s.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kmm.sigs.x-k8s.io,resources=modules/status,verbs=get

// KMMReconciler manages a KMM Module CR for out-of-tree kernel module loading.
// It only configures the moduleLoader section — DP/DRA lifecycle remains with the native controllers.
type KMMReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

const (
	kmmModuleSuffix = "-gpu"

	kmmNotEnabledMsg = "KMM is not installed in the cluster."
)

func kmmModuleName(cpName string) string {
	return cpName + kmmModuleSuffix
}

func (r *KMMReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	moduleName := kmmModuleName(r.Opts.ReqName)

	if !r.Opts.KMMEnable {
		if cp != nil {
			addIfMissing(&cp.Status.Errors, kmmNotEnabledMsg)
		}

		return ctrl.Result{}, nil
	}

	if cp == nil || cp.Spec.KernelModule == nil {
		if cp != nil {
			cp.Status.KMMStatus = notAvailableStatus
		}

		return r.deleteModuleIfExists(ctx, moduleName)
	}

	if r.Opts.OpenShift {
		if err := r.ensureOpenShiftSCC(ctx, cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set up module-loader SCC: %w", err)
		}
	}

	mod := &kmmv1beta1.Module{
		ObjectMeta: metav1.ObjectMeta{
			Name:      moduleName,
			Namespace: r.Opts.Namespace,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, mod, func() error {
		return r.setModuleDesiredState(mod, cp)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile KMM Module %s for ClusterPolicy %s: %w", moduleName, cp.Name, err)
	}

	klog.Infof("KMM Module %s %s", moduleName, result)

	r.updateStatus(cp, mod)

	return ctrl.Result{}, nil
}

func (r *KMMReconciler) setModuleDesiredState(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) error {
	if err := ctrl.SetControllerReference(cp, mod, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	mod.Spec.Selector = generateNodeSelector(cp)
	mod.Spec.Tolerations = generateTolerations(cp)
	mod.Spec.ImageRepoSecret = cp.Spec.PullSecret

	r.setModuleLoader(mod, cp)

	return nil
}

func (r *KMMReconciler) setModuleLoader(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	km := cp.Spec.KernelModule

	container := kmmv1beta1.ModuleLoaderContainerSpec{
		Modprobe: kmmv1beta1.ModprobeSpec{
			ModuleName:          km.ModuleName,
			FirmwarePath:        km.FirmwarePath,
			ModulesLoadingOrder: km.ModulesLoadingOrder,
		},
		ContainerImage:        km.Image,
		InTreeModulesToRemove: dedupeStrings(append([]string{km.ModuleName}, km.InTreeModulesToRemove...)),
		ImagePullPolicy:       v1.PullAlways,
		RegistryTLS: kmmv1beta1.TLSOptions{
			InsecureSkipTLSVerify: km.SkipTLSVerify,
		},
	}

	if len(km.KernelMappings) == 0 {
		container.KernelMappings = []kmmv1beta1.KernelMapping{
			{Regexp: "^.+$"},
		}
	} else {
		mappings := make([]kmmv1beta1.KernelMapping, 0, len(km.KernelMappings))
		for _, m := range km.KernelMappings {
			mapping := kmmv1beta1.KernelMapping{
				Regexp:                m.Regexp,
				Literal:               m.Literal,
				ContainerImage:        m.ContainerImage,
				InTreeModulesToRemove: m.InTreeModulesToRemove,
			}
			if m.Build != nil {
				mapping.Build = convertBuildSpec(m.Build)
			}
			mappings = append(mappings, mapping)
		}
		container.KernelMappings = mappings
	}

	mod.Spec.ModuleLoader = &kmmv1beta1.ModuleLoaderSpec{
		Container:          container,
		ServiceAccountName: r.Opts.ModuleLoaderServiceAccountName,
	}
}

func convertBuildSpec(src *v1alpha.KernelModuleBuildSpec) *kmmv1beta1.Build {
	build := &kmmv1beta1.Build{
		DockerfileConfigMap: &v1.LocalObjectReference{Name: src.DockerfileConfigMap.Name},
		Secrets:             src.Secrets,
	}

	if len(src.BuildArgs) > 0 {
		args := make([]kmmv1beta1.BuildArg, len(src.BuildArgs))
		for i, a := range src.BuildArgs {
			args[i] = kmmv1beta1.BuildArg{Name: a.Name, Value: a.Value}
		}
		build.BuildArgs = args
	}

	return build
}

func dedupeStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	result := make([]string, 0, len(s))

	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}

	return result
}

func (r *KMMReconciler) deleteModuleIfExists(ctx context.Context, name string) (ctrl.Result, error) {
	mod := &kmmv1beta1.Module{}
	key := types.NamespacedName{Name: name, Namespace: r.Opts.Namespace}

	if err := r.Get(ctx, key, mod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get KMM Module %s: %w", name, err)
	}

	klog.Infof("Deleting KMM Module %s", name)

	if err := r.Delete(ctx, mod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete KMM Module %s: %w", name, err)
	}

	return ctrl.Result{}, nil
}

func (r *KMMReconciler) ensureOpenShiftSCC(ctx context.Context, cp *v1alpha.ClusterPolicy) error {
	sccName, roleName, bindingName, _ := buildOpenShiftNames(cp.Name, "module-loader")
	saName := r.Opts.ModuleLoaderServiceAccountName

	if err := createServiceAccount(ctx, r.Client, saName, r.Opts.Namespace); err != nil {
		return err
	}

	scc := buildModuleLoaderSCC(sccName)
	if err := ensureSCC(ctx, r.Client, scc); err != nil {
		return err
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		return err
	}

	return createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace)
}

func (r *KMMReconciler) updateStatus(cp *v1alpha.ClusterPolicy, mod *kmmv1beta1.Module) {
	if cp.Spec.KernelModule != nil {
		mlStatus := mod.Status.ModuleLoader
		cp.Status.KMMStatus = fmt.Sprintf("%d/%d", mlStatus.AvailableNumber, mlStatus.DesiredNumber)
	} else {
		cp.Status.KMMStatus = notAvailableStatus
	}
}
