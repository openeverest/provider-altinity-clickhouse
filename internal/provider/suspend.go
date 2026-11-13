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

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// Suspending an Instance (spec.suspend.suspended) stops its compute and keeps
// its volumes: the provider sets the operator's spec.stop on the
// ClickHouseInstallation and, in the replicated topology, the
// ClickHouseKeeperInstallation. The operator scales their StatefulSets to 0
// and removes the client Service. ClickHouse stops before Keeper, since its
// replicated tables need Keeper while it runs, and on resume Keeper starts
// first (the usual order in syncReplicated). The runtime rejects spec changes
// and holds maintenance and backups while the Instance is suspended, and
// reports Suspending and Resuming around the Suspended phase Status reports.

// suspended reports whether the Instance asks to be suspended.
func suspended(c *controller.Context) bool {
	return c.Instance().SuspendRequested()
}

// stopSwitch is the operator's stop switch: set while the Instance is
// suspended, absent otherwise, so a running cluster renders exactly as it did
// before suspending existed and nothing restarts on upgrade.
func stopSwitch(c *controller.Context) *chtypes.StringBool {
	if suspended(c) {
		return chtypes.NewStringBool(true)
	}
	return nil
}

// suspendReplicated stops ClickHouse, then Keeper.
func suspendReplicated(c *controller.Context, gate *rolloutGate, chi *chiv1.ClickHouseInstallation, chk *chkv1.ClickHouseKeeperInstallation) error {
	if err := applyAndWaitForCHI(c, gate, chi); err != nil {
		return err
	}
	if _, err := gate.apply(c, chk, "chk"); err != nil {
		return fmt.Errorf("apply ClickHouseKeeperInstallation: %w", err)
	}
	live := &chkv1.ClickHouseKeeperInstallation{}
	if err := c.Get(live, keeperCRName(c.Name())); err != nil {
		return fmt.Errorf("read ClickHouseKeeperInstallation: %w", err)
	}
	return waitForCHK(c, live)
}

// suspendStatus reports Suspended once the operator has stopped every
// installation of the Instance and no pod is left, Suspending before.
func suspendStatus(c *controller.Context, chi *chiv1.ClickHouseInstallation, chk *chkv1.ClickHouseKeeperInstallation) (controller.Status, error) {
	if !stopped(chi.Spec.Stop, chiInstallation(chi)) {
		return controller.Suspending("Stopping ClickHouse"), nil
	}
	if n, err := countPods(c, client.MatchingLabels{common.LabelCHIName: c.Name()}); err != nil || n > 0 {
		return controller.Suspending(fmt.Sprintf("Stopping ClickHouse: %d pods left", n)), err
	}
	if chk == nil {
		return controller.Suspended(), nil
	}
	if !stopped(chk.Spec.Stop, chkInstallation(chk)) {
		return controller.Suspending("Stopping ClickHouse Keeper"), nil
	}
	if n, err := countPods(c, client.MatchingLabels{common.LabelCHKName: chk.Name}); err != nil || n > 0 {
		return controller.Suspending(fmt.Sprintf("Stopping ClickHouse Keeper: %d pods left", n)), err
	}
	return controller.Suspended(), nil
}

// stopped reports whether the operator has reconciled an installation with
// its stop switch on: installation reads InProgress until it has picked up
// the change that set it.
func stopped(stop *chtypes.StringBool, i installation) bool {
	return stop.Value() && i.exists && i.state == chiv1.StatusCompleted
}

// countPods counts the Instance's pods matching labels.
func countPods(c *controller.Context, labels client.MatchingLabels) (int, error) {
	pods := &corev1.PodList{}
	if err := c.List(pods, labels); err != nil {
		return 0, fmt.Errorf("list pods: %w", err)
	}
	return len(pods.Items), nil
}
