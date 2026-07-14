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
	"strings"

	v1 "k8s.io/api/core/v1"
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

	healthCheckPort    = 51516
	gpuDeviceClass     = "gpu.intel.com"
	vfioGpuDeviceClass = "gpu-vfio.intel.com"

	kmmNotEnabledMsg = "KMM is not installed in the cluster."
)

var hostPathDirOrCreate = v1.HostPathDirectoryOrCreate

func logLevelForDp(spec *v1alpha.ClusterPolicy) int32 {
	return max(spec.Spec.LogLevel, spec.Spec.DevicePluginSpec.LogLevel)
}

func hexArgStr(s []string) string {
	a := make([]string, 0, len(s))
	for _, str := range s {
		if !strings.HasPrefix(str, "0x") {
			str = "0x" + str
		}

		a = append(a, str)
	}
	return strings.Join(a, ",")
}

func dpArgs(spec *v1alpha.ClusterPolicy) []string {
	var args []string

	dpspec := spec.Spec.DevicePluginSpec

	if spec.Spec.ResourceMonitoring {
		args = append(args,
			"-enable-monitoring",
			"-xpumd-endpoint=/run/xpumd/intelxpuinfo.sock")
	}

	logLevel := logLevelForDp(spec)
	if logLevel > 0 {
		args = append(args, fmt.Sprintf("-v=%d", logLevel))
	}

	if len(dpspec.ByPathMode) > 0 {
		args = append(args, fmt.Sprintf("-bypath=%s", dpspec.ByPathMode))
	}

	if len(dpspec.AllowIDs) > 0 {
		args = append(args, fmt.Sprintf("-allow-ids=%s", hexArgStr(dpspec.AllowIDs)))
	}

	if len(dpspec.DenyIDs) > 0 {
		args = append(args, fmt.Sprintf("-deny-ids=%s", hexArgStr(dpspec.DenyIDs)))
	}

	return args
}

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

	if !r.Opts.DRAEnable && cp != nil && cp.Spec.ResourceRegistration == resourceModeDRA {
		addIfMissing(&cp.Status.Errors, "DRA is not enabled in the cluster, but ClusterPolicy requests it.")
		return ctrl.Result{}, nil
	}

	if cp == nil || !cp.DeletionTimestamp.IsZero() {
		return r.deleteModuleIfExists(ctx, moduleName)
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
	return gpuNodeSelector(cp)
}

func (r *KMMReconciler) setModuleLoader(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	if cp.Spec.KernelModule == nil {
		mod.Spec.ModuleLoader = nil
		return
	}

	km := cp.Spec.KernelModule

	mod.Spec.ModuleLoader = &kmmv1beta1.ModuleLoaderSpec{
		Container: kmmv1beta1.ModuleLoaderContainerSpec{
			Modprobe: kmmv1beta1.ModprobeSpec{
				ModuleName: km.ModuleName,
			},
			KernelMappings: []kmmv1beta1.KernelMapping{
				{
					Regexp:                "^.+$",
					ContainerImage:        km.Image,
					InTreeModulesToRemove: []string{km.ModuleName},
				},
			},
			ImagePullPolicy: v1.PullAlways,
		},
		ServiceAccountName: r.Opts.ModuleLoaderServiceAccountName,
	}
}

func (r *KMMReconciler) setDRA(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	mod.Spec.DevicePlugin = nil

	celExpression := fmt.Sprintf("device.driver == %q", gpuDeviceClass)

	args := r.generateDRAArgs(cp)

	// KMM's DRA reconciler automatically adds: kubelet-plugins, kubelet-plugins-registry,
	// cdi (/var/run/cdi) volumes/mounts and NODE_NAME, CDI_ROOT, POD_UID env vars.
	// We only include what KMM doesn't provide.
	mod.Spec.DRA = &kmmv1beta1.DRASpec{
		DriverName:         gpuDeviceClass,
		ServiceAccountName: r.Opts.DRAServiceAccountName,
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
// KMM auto-adds: kubelet-plugins, kubelet-plugins-registry, cdi (/var/run/cdi).
func draExtraVolumes() []v1.Volume {
	return []v1.Volume{
		{Name: "sysfs", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/sys"}}},
		{Name: "xpumdrundir", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/run/xpumd", Type: &hostPathDirOrCreate}}},
	}
}

func (r *KMMReconciler) setDevicePlugin(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	mod.Spec.DRA = nil

	mod.Spec.DevicePlugin = &kmmv1beta1.DevicePluginSpec{
		ServiceAccountName: r.Opts.DPServiceAccountName,
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
				{Name: "cdipath", MountPath: "/var/run/cdi"},
			},
		},
		Volumes: []v1.Volume{
			{Name: "devfs", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev/dri"}}},
			{Name: "sysfsdrm", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/sys/class/drm"}}},
			{Name: "cdipath", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/var/run/cdi", Type: &hostPathDirOrCreate}}},
		},
	}

	if cp.Spec.ResourceMonitoring {
		mod.Spec.DevicePlugin.Volumes = append(mod.Spec.DevicePlugin.Volumes, v1.Volume{
			Name: xpumdVolumeName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/run/xpumd",
					Type: &hostPathDirOrCreate,
				},
			},
		})
		mod.Spec.DevicePlugin.Container.VolumeMounts = append(mod.Spec.DevicePlugin.Container.VolumeMounts, v1.VolumeMount{
			Name:      xpumdVolumeName,
			MountPath: "/run/xpumd",
		})
	}
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

	if mod.Spec.DRA != nil && r.anyAllocatedResourceClaims(ctx, mod.Spec.DRA.DriverName) {
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, requeueReconcileErr{}
	}

	klog.Infof("Deleting KMM Module %s", name)

	if err := r.Delete(ctx, mod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete KMM Module %s: %w", name, err)
	}

	return ctrl.Result{}, nil
}

func (r *KMMReconciler) anyAllocatedResourceClaims(ctx context.Context, driverName string) bool {
	var rcList resourcev1.ResourceClaimList

	klog.Info("Checking for allocated ResourceClaims that would prevent DRA removal")

	// Fail safe: assume claims exist so we don't delete the Module while pods may still be using GPUs.
	if err := r.List(ctx, &rcList); err != nil {
		klog.Error(err, "unable to list ResourceClaims, assuming allocated claims exist")
		return true
	}

	for _, claim := range rcList.Items {
		alloc := claim.Status.Allocation
		if alloc == nil || len(alloc.Devices.Results) == 0 {
			continue
		}

		for _, dev := range alloc.Devices.Results {
			if dev.Driver == driverName {
				klog.Infof("Found allocated ResourceClaim with GPU device: %s", claim.Name)
				return true
			}
		}
	}

	return false
}

func (r *KMMReconciler) updateStatus(cp *v1alpha.ClusterPolicy, mod *kmmv1beta1.Module) {
	switch cp.Spec.ResourceRegistration {
	case resourceModeDRA:
		dsStatus := mod.Status.DRA
		cp.Status.DRAStatus = fmt.Sprintf("%d/%d", dsStatus.AvailableNumber, dsStatus.DesiredNumber)
		cp.Status.DevicePluginStatus = notAvailableStatus
	case resourceModeDP:
		dsStatus := mod.Status.DevicePlugin
		cp.Status.DevicePluginStatus = fmt.Sprintf("%d/%d", dsStatus.AvailableNumber, dsStatus.DesiredNumber)
		cp.Status.DRAStatus = notAvailableStatus
	}
}
