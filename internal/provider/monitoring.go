// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"fmt"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// reconcilePodMonitor creates or removes the Prometheus PodMonitor for the
// instance based on the engine's podMonitor parameter. The native metrics
// endpoint is always exposed by the CHI; this only manages the PodMonitor.
// When the prometheus-operator CRDs are absent, PodMonitor operations are
// silently skipped so clusters without them still reconcile.
func reconcilePodMonitor(c *controller.Context) error {
	l := log.FromContext(c.Context())

	if podMonitorEnabled(c) {
		if err := c.Apply(buildPodMonitor(c)); err != nil {
			if meta.IsNoMatchError(err) {
				l.Info("prometheus-operator CRDs not installed, skipping PodMonitor", "name", c.Name())
				return nil
			}
			return fmt.Errorf("apply PodMonitor: %w", err)
		}
		return nil
	}

	return deletePodMonitor(c)
}

// deletePodMonitor removes the instance's PodMonitor, ignoring a missing CRD.
func deletePodMonitor(c *controller.Context) error {
	pm := &monitoringv1.PodMonitor{ObjectMeta: c.ObjectMeta(podMonitorName(c.Name()))}
	if err := c.Delete(pm); err != nil && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete PodMonitor: %w", err)
	}
	return nil
}

// podMonitorEnabled reports whether the engine's podMonitor parameter is enabled.
func podMonitorEnabled(c *controller.Context) bool {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	var params components.ClickHouseParameters
	_ = c.TryDecodeComponentParameters(engine, &params)
	return strings.EqualFold(params.PodMonitor, common.PodMonitorEnabled)
}

// buildPodMonitor constructs a PodMonitor selecting the instance's ClickHouse
// pods and scraping the native Prometheus metrics port.
func buildPodMonitor(c *controller.Context) *monitoringv1.PodMonitor {
	port := common.MetricsPortName
	return &monitoringv1.PodMonitor{
		TypeMeta: metav1.TypeMeta{
			APIVersion: monitoringv1.SchemeGroupVersion.String(),
			Kind:       monitoringv1.PodMonitorsKind,
		},
		ObjectMeta: c.ObjectMeta(podMonitorName(c.Name())),
		Spec: monitoringv1.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{common.LabelCHIName: c.Name()},
			},
			PodMetricsEndpoints: []monitoringv1.PodMetricsEndpoint{
				{Port: &port, Path: common.MetricsPath},
			},
		},
	}
}

// podMonitorName returns the PodMonitor resource name for an instance.
func podMonitorName(instanceName string) string {
	return instanceName + "-metrics"
}
