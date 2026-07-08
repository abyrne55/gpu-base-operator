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
	"strconv"

	v1 "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
)

// +kubebuilder:rbac:groups=kmm.sigs.x-k8s.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kmm.sigs.x-k8s.io,resources=modules/status,verbs=get

// KMMReconciler manages a KMM Module CR that delegates DP/DRA DaemonSet lifecycle
// to the Kernel Module Management operator, replacing direct DaemonSet management.
type KMMReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

const (
	kmmModuleSuffix = "-gpu"
	gpuDriverModule = "xe"

	kmmDRAResourcePart = "kmm-dra"

	kmmNotEnabledMsg = "KMM is not installed in the cluster, but ClusterPolicy requests it (spec.kmm is set)."
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

	if cp == nil || !cp.DeletionTimestamp.IsZero() || cp.Spec.KMM == nil {
		return r.deleteModuleIfExists(ctx, moduleName)
	}

	if cp.Spec.ResourceRegistration == resourceModeDRA {
		if err := r.ensureDRARBAC(ctx, cp.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to ensure DRA RBAC for KMM: %w", err)
		}

		if r.Opts.OpenShift {
			if err := r.ensureOpenShiftDRASCC(ctx, cp.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to ensure OpenShift DRA SCC for KMM: %w", err)
			}
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

	mod.Spec.Selector = r.buildNodeSelector(cp)

	mod.Spec.ImageRepoSecret = cp.Spec.PullSecret
	mod.Spec.Tolerations = cp.Spec.Tolerations

	r.setModuleLoader(mod, cp)

	switch cp.Spec.ResourceRegistration {
	case resourceModeDRA:
		r.setDRA(mod, cp)
	case resourceModeDP:
		r.setDevicePlugin(mod, cp)
	}

	return nil
}

func (r *KMMReconciler) buildNodeSelector(cp *v1alpha.ClusterPolicy) map[string]string {
	selector := map[string]string{
		"kubernetes.io/arch": "amd64",
	}

	for k, v := range cp.Spec.NodeSelector {
		selector[k] = v
	}

	if cp.Spec.UseNFDLabeling {
		selector["intel.feature.node.kubernetes.io/gpu"] = trueValue
	}

	return selector
}

func (r *KMMReconciler) setModuleLoader(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	if cp.Spec.KMM.UseInTreeDriver {
		mod.Spec.ModuleLoader = nil
		return
	}

	mod.Spec.ModuleLoader = &kmmv1beta1.ModuleLoaderSpec{
		Container: kmmv1beta1.ModuleLoaderContainerSpec{
			Modprobe: kmmv1beta1.ModprobeSpec{
				ModuleName: gpuDriverModule,
			},
			KernelMappings: []kmmv1beta1.KernelMapping{
				{
					Regexp:                "^.+$",
					ContainerImage:        cp.Spec.KMM.DriverImage,
					InTreeModulesToRemove: []string{gpuDriverModule},
				},
			},
			ImagePullPolicy: v1.PullAlways,
			Version:         cp.Spec.KMM.DriverVersion,
		},
	}
}

func (r *KMMReconciler) setDRA(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	mod.Spec.DevicePlugin = nil

	celExpression := fmt.Sprintf("device.driver == %q", gpuDeviceClass)

	args := r.generateDRAArgs(cp)

	// KMM's DRA reconciler automatically adds: kubelet-plugins, kubelet-plugins-registry,
	// cdi (/var/run/cdi) volumes/mounts and NODE_NAME, CDI_ROOT, POD_UID env vars.
	// We only include what KMM doesn't provide.
	_, _, _, saName := buildOpenShiftNames(cp.Name, kmmDRAResourcePart)

	mod.Spec.DRA = &kmmv1beta1.DRASpec{
		DriverName:         gpuDeviceClass,
		ServiceAccountName: saName,
		Container: kmmv1beta1.CommonContainerSpec{
			Image:           cp.Spec.DynamicResourceAllocationSpec.Image,
			ImagePullPolicy: v1.PullIfNotPresent,
			Command:         []string{"/kubelet-gpu-plugin"},
			Args:            args,
			Env: []v1.EnvVar{
				{
					Name: "POD_NAMESPACE",
					ValueFrom: &v1.EnvVarSource{
						FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
					},
				},
				{
					Name:  "SYSFS_ROOT",
					Value: "/sysfs",
				},
			},
			VolumeMounts: []v1.VolumeMount{
				{Name: "etccdi", MountPath: "/etc/cdi"},
				{Name: "xpumdrundir", MountPath: "/var/run/xpumd"},
				{Name: "sysfs", MountPath: "/sysfs"},
			},
		},
		Volumes: draExtraVolumes(),
		DeviceClasses: []kmmv1beta1.DeviceClassSpec{
			{
				Name: gpuDeviceClass,
				Selectors: []resourcev1.DeviceSelector{
					{CEL: &resourcev1.CELDeviceSelector{Expression: celExpression}},
				},
			},
			{
				Name: vfioGpuDeviceClass,
				Selectors: []resourcev1.DeviceSelector{
					{CEL: &resourcev1.CELDeviceSelector{Expression: celExpression}},
				},
			},
		},
	}
}

func (r *KMMReconciler) generateDRAArgs(cp *v1alpha.ClusterPolicy) []string {
	targetLevel := max(cp.Spec.DynamicResourceAllocationSpec.LogLevel, cp.Spec.LogLevel)

	args := []string{fmt.Sprintf("-v=%d", targetLevel)}

	if cp.Spec.HealthinessSpec != nil {
		args = append(args, "--health-monitoring=true")
	}

	if cp.Spec.DynamicResourceAllocationSpec.PodHealthCheck {
		args = append(args, fmt.Sprintf("--healthcheck-port=%d", healthCheckPort))
	} else {
		args = append(args, "--healthcheck-port=-1")
	}

	args = append(args, fmt.Sprintf("--manage-binding=%s", strconv.FormatBool(cp.Spec.DynamicResourceAllocationSpec.ManageBinding)))

	return args
}

// draExtraVolumes returns only the volumes KMM's DRA reconciler doesn't add automatically.
// KMM adds: kubelet-plugins, kubelet-plugins-registry, cdi (/var/run/cdi).
func draExtraVolumes() []v1.Volume {
	directoryOrCreate := v1.HostPathDirectoryOrCreate
	return []v1.Volume{
		{Name: "etccdi", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/opt/cdi", Type: &directoryOrCreate}}},
		{Name: "sysfs", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/sys"}}},
		{Name: "xpumdrundir", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/run/xpumd", Type: &directoryOrCreate}}},
	}
}

func (r *KMMReconciler) setDevicePlugin(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	mod.Spec.DRA = nil

	directoryOrCreate := v1.HostPathDirectoryOrCreate

	mod.Spec.DevicePlugin = &kmmv1beta1.DevicePluginSpec{
		Container: kmmv1beta1.CommonContainerSpec{
			Image:           cp.Spec.DevicePluginSpec.PluginImage,
			ImagePullPolicy: v1.PullIfNotPresent,
			Args:            dpArgs(cp),
			Env: []v1.EnvVar{
				{
					Name: "NODE_NAME",
					ValueFrom: &v1.EnvVarSource{
						FieldRef: &v1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
					},
				},
				{
					Name: "HOST_IP",
					ValueFrom: &v1.EnvVarSource{
						FieldRef: &v1.ObjectFieldSelector{FieldPath: "status.hostIP"},
					},
				},
			},
			VolumeMounts: []v1.VolumeMount{
				{Name: "devfs", MountPath: "/dev/dri", ReadOnly: true},
				{Name: "sysfsdrm", MountPath: "/sys/class/drm", ReadOnly: true},
				{Name: "kubeletsockets", MountPath: "/var/lib/kubelet/device-plugins"},
				{Name: "cdipath", MountPath: "/var/run/cdi"},
			},
		},
		Volumes: []v1.Volume{
			{Name: "devfs", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev/dri"}}},
			{Name: "sysfsdrm", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/sys/class/drm"}}},
			{Name: "kubeletsockets", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}}},
			{Name: "cdipath", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/run/cdi", Type: &directoryOrCreate}}},
		},
	}

	if cp.Spec.ResourceMonitoring {
		mod.Spec.DevicePlugin.Volumes = append(mod.Spec.DevicePlugin.Volumes, v1.Volume{
			Name: xpumdVolumeName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/run/xpumd",
					Type: &directoryOrCreate,
				},
			},
		})
		mod.Spec.DevicePlugin.Container.VolumeMounts = append(mod.Spec.DevicePlugin.Container.VolumeMounts, v1.VolumeMount{
			Name:      xpumdVolumeName,
			MountPath: "/run/xpumd",
		})
	}
}

func (r *KMMReconciler) ensureDRARBAC(ctx context.Context, crName string) error {
	_, _, _, saName := buildOpenShiftNames(crName, kmmDRAResourcePart)
	rbacName := crName + "-" + kmmDRAResourcePart

	if err := createServiceAccount(ctx, r.Client, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure KMM DRA ServiceAccount: %w", err)
	}

	cr := &rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: rbacName}}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, cr, func() error {
		desired := deployments.DynamicResourceAllocationClusterRole()
		cr.Rules = desired.Rules
		return nil
	}); err != nil {
		return fmt.Errorf("failed to ensure KMM DRA ClusterRole: %w", err)
	}

	crb := &rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: rbacName}}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, crb, func() error {
		crb.Subjects = []rbac.Subject{{
			Kind:      rbac.ServiceAccountKind,
			Name:      saName,
			Namespace: r.Opts.Namespace,
		}}
		crb.RoleRef = rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "ClusterRole",
			Name:     rbacName,
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to ensure KMM DRA ClusterRoleBinding: %w", err)
	}

	return nil
}

func (r *KMMReconciler) ensureOpenShiftDRASCC(ctx context.Context, crName string) error {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(crName, kmmDRAResourcePart)

	if err := ensureSCC(ctx, r.Client, buildKMMDRASCC(sccName)); err != nil {
		return fmt.Errorf("failed to ensure KMM DRA SCC: %w", err)
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		return fmt.Errorf("failed to ensure KMM DRA SCC ClusterRole: %w", err)
	}

	if err := createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure KMM DRA SCC ClusterRoleBinding: %w", err)
	}

	return nil
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

	wasDRA := mod.Spec.DRA != nil

	if err := r.Delete(ctx, mod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete KMM Module %s: %w", name, err)
	}

	if wasDRA {
		r.cleanupDRARBAC(ctx, r.Opts.ReqName)
	}

	return ctrl.Result{}, nil
}

func (r *KMMReconciler) cleanupDRARBAC(ctx context.Context, crName string) {
	sccName, sccRoleName, sccBindingName, saName := buildOpenShiftNames(crName, kmmDRAResourcePart)
	rbacName := crName + "-" + kmmDRAResourcePart

	cr := &rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: rbacName}}
	if err := r.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Failed to delete KMM DRA ClusterRole %s: %v", rbacName, err)
	}

	crb := &rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: rbacName}}
	if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Failed to delete KMM DRA ClusterRoleBinding %s: %v", rbacName, err)
	}

	if r.Opts.OpenShift {
		deleteOpenShiftSCCResources(ctx, r.Client, sccName, sccRoleName, sccBindingName, saName, r.Opts.Namespace)
	} else {
		// On non-OpenShift, just delete the ServiceAccount
		sa := &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: r.Opts.Namespace}}
		if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
			klog.Errorf("Failed to delete KMM DRA ServiceAccount %s: %v", saName, err)
		}
	}
}

func (r *KMMReconciler) updateStatus(cp *v1alpha.ClusterPolicy, mod *kmmv1beta1.Module) {
	switch cp.Spec.ResourceRegistration {
	case resourceModeDRA:
		dsStatus := mod.Status.DRA
		cp.Status.DRAStatus = fmt.Sprintf("%d/%d", dsStatus.AvailableNumber, dsStatus.DesiredNumber)
		cp.Status.DevicePluginStatus = notAvailableStatus
		cp.Status.KMMModuleStatus = daemonSetStatusSummary(dsStatus)
	case resourceModeDP:
		dsStatus := mod.Status.DevicePlugin
		cp.Status.DevicePluginStatus = fmt.Sprintf("%d/%d", dsStatus.AvailableNumber, dsStatus.DesiredNumber)
		cp.Status.DRAStatus = notAvailableStatus
		cp.Status.KMMModuleStatus = daemonSetStatusSummary(dsStatus)
	}
}

func daemonSetStatusSummary(ds kmmv1beta1.DaemonSetStatus) string {
	if ds.DesiredNumber == 0 {
		return "Pending"
	}

	if ds.AvailableNumber == ds.DesiredNumber {
		return "Ready"
	}

	return fmt.Sprintf("Progressing (%d/%d)", ds.AvailableNumber, ds.DesiredNumber)
}
