// Copyright 2025 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployments

import (
	"testing"

	core "k8s.io/api/core/v1"
)

const (
	allCaps = "ALL"
)

// findContainer returns a pointer to the named container, or nil if not found.
func findContainer(containers []core.Container, name string) *core.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

// findInitContainer returns a pointer to the named init container, or nil if not found.
func findInitContainer(containers []core.Container, name string) *core.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func TestXpuManagerDaemonset(t *testing.T) {
	ds := XpuManagerDaemonset()
	if ds == nil {
		t.Error("XpuManagerDaemonset returned nil")
	}
}

func TestDynamicResourceAllocationMonitorClaimTemplate(t *testing.T) {
	mct := DynamicResourceAllocationMonitorClaimTemplate()
	if mct == nil {
		t.Error("DynamicResourceAllocationMonitorClaimTemplate returned nil")
	}
}

func TestNFDNodeFeatureRulesGpu(t *testing.T) {
	rule := NFDNodeFeatureRulesGpu()
	if rule == nil {
		t.Error("NFDNodeFeatureRulesGpu returned nil")
	}
}

func TestPrometheusServiceMonitor(t *testing.T) {
	sm := PrometheusServiceMonitor()
	if sm == nil {
		t.Error("PrometheusServiceMonitor returned nil")
	}
}

func TestXpuManagerService(t *testing.T) {
	svc := XpuManagerService()
	if svc == nil {
		t.Error("XpuManagerService returned nil")
	}
}

func TestXpuFwUpdateJob(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	if job == nil {
		t.Error("XpuManagerFWUpdateJob returned nil")
	}
}

func TestOTelConfig(t *testing.T) {
	cfg := XpuManagerOTelConfig()
	if cfg == nil {
		t.Error("XpuManagerOTelConfig returned nil")
	}
}

func TestXpuManagerDaemonset_AutomountServiceAccountToken(t *testing.T) {
	ds := XpuManagerDaemonset()
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("automountServiceAccountToken must be set (non-nil)")
	}
	if *ds.Spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestXpuFwUpdateJob_AutomountServiceAccountToken(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("automountServiceAccountToken must be set (non-nil)")
	}
	if *job.Spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestXpuFwUpdateJob_FwCopyInitContainerSecurityContext(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	c := findInitContainer(job.Spec.Template.Spec.InitContainers, "fw-copy")
	if c == nil {
		t.Fatal("fw-copy init container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on fw-copy")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil {
		t.Fatal("AllowPrivilegeEscalation must be set (non-nil) on fw-copy")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation != false {
		t.Error("AllowPrivilegeEscalation must be false on fw-copy")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on fw-copy")
	}
	found := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			found = true
			break
		}
	}
	if !found {
		t.Error("capabilities.drop must contain ALL on fw-copy")
	}
	if c.SecurityContext.SeccompProfile == nil {
		t.Fatal("SeccompProfile must be set on fw-copy")
	}
	if c.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile.Type: got %v, want RuntimeDefault on fw-copy", c.SecurityContext.SeccompProfile.Type)
	}
}

func TestXpuFwUpdateJob_UpdaterContainerSecurityContext(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	c := findContainer(job.Spec.Template.Spec.Containers, "updater")
	if c == nil {
		t.Fatal("updater container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on updater")
	}
	if c.SecurityContext.SeccompProfile == nil {
		t.Fatal("SeccompProfile must be set on updater")
	}
	if c.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile.Type: got %v, want RuntimeDefault on updater", c.SecurityContext.SeccompProfile.Type)
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on updater")
	}
	found := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			found = true
			break
		}
	}
	if !found {
		t.Error("capabilities.drop must contain ALL on updater")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		t.Fatal("ReadOnlyRootFilesystem must be set (non-nil) on updater")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true on updater")
	}
}

func TestXpuManagerDaemonset_XpumdContainerSecurityContext(t *testing.T) {
	ds := XpuManagerDaemonset()
	c := findContainer(ds.Spec.Template.Spec.Containers, "xpumd")
	if c == nil {
		t.Fatal("xpumd container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on xpumd")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil {
		t.Fatal("AllowPrivilegeEscalation must be set (non-nil) on xpumd")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false on xpumd")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		t.Fatal("ReadOnlyRootFilesystem must be set (non-nil) on xpumd")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true on xpumd")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on xpumd")
	}
	foundDrop := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			foundDrop = true
			break
		}
	}
	if !foundDrop {
		t.Error("capabilities.drop must contain ALL on xpumd")
	}
	foundAdd := false
	for _, cap := range c.SecurityContext.Capabilities.Add {
		if cap == "SYS_ADMIN" {
			foundAdd = true
			break
		}
	}
	if !foundAdd {
		t.Error("capabilities.add must contain SYS_ADMIN on xpumd (required for engine utilization metrics)")
	}
}
