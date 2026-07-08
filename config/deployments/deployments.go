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
	_ "embed"

	apps "k8s.io/api/apps/v1"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"

	prometheusv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	resv1 "k8s.io/api/resource/v1"
	nfdcrd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	"sigs.k8s.io/yaml"
)

// XPU Manager

//go:embed xpum/xpum.yaml
var contentXpumDs []byte

func XpuManagerDaemonset() *apps.DaemonSet {
	return getDaemonset(contentXpumDs).DeepCopy()
}

//go:embed xpum/otel-config.yaml
var contentXpumOTelConfig []byte

func XpuManagerOTelConfig() *OTelConfig {
	return getOTelConfig(contentXpumOTelConfig)
}

// DRA

//go:embed dra/monitorclaimtemplate.yaml
var contentDRAMCT []byte

func DynamicResourceAllocationMonitorClaimTemplate() *resv1.ResourceClaimTemplate {
	return getResourceClaimTemplate(contentDRAMCT).DeepCopy()
}

// NFD

//go:embed nfd/node-feature-rules-gpu.yaml
var nfdNodeFeatureRulesGpu []byte

func NFDNodeFeatureRulesGpu() *nfdcrd.NodeFeatureRule {
	return getNodeFeatureRule(nfdNodeFeatureRulesGpu).DeepCopy()
}

// Prometheus

//go:embed prometheus/service-monitor.yaml
var prometheusServiceMonitor []byte

func PrometheusServiceMonitor() *prometheusv1.ServiceMonitor {
	return getServiceMonitor(prometheusServiceMonitor).DeepCopy()
}

//go:embed xpum/service.yaml
var xpumService []byte

func XpuManagerService() *core.Service {
	return getService(xpumService).DeepCopy()
}

//go:embed xpum/xpum-fwupdate-job.yaml
var xpumFWUpdateJob []byte

func XpuManagerFWUpdateJob() *batch.Job {
	return getJob(xpumFWUpdateJob).DeepCopy()
}

// generic functions

func getDaemonset(content []byte) *apps.DaemonSet {
	var result apps.DaemonSet

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getService(content []byte) *core.Service {
	var result core.Service

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getResourceClaimTemplate(content []byte) *resv1.ResourceClaimTemplate {
	var result resv1.ResourceClaimTemplate

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getNodeFeatureRule(content []byte) *nfdcrd.NodeFeatureRule {
	var result nfdcrd.NodeFeatureRule

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getServiceMonitor(content []byte) *prometheusv1.ServiceMonitor {
	var result prometheusv1.ServiceMonitor

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getJob(content []byte) *batch.Job {
	var result batch.Job

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

// getOTelConfig parses the embedded otel-config.yaml into an OTelConfig struct.
func getOTelConfig(content []byte) *OTelConfig {
	var result OTelConfig

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}
