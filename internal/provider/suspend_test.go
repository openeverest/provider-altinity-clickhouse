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
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// suspend returns a copy of instance asking to be suspended.
func suspend(instance *corev1alpha1.Instance) *corev1alpha1.Instance {
	out := instance.DeepCopy()
	out.Spec.Suspend = &corev1alpha1.SuspendSpec{Suspended: true}
	return out
}

func TestProviderDeclaresSuspend(t *testing.T) {
	assert.NotNil(t, testProvider(t).Spec.Suspend, "the runtime rejects suspending without Provider.spec.suspend")

	v := validationtest.New(t, New(), providerSpecPath)
	for _, fixture := range []string{standaloneFixture, replicatedFixture} {
		res := v.Run(suspend(validationtest.Load(t, fixture)), nil)
		assert.Empty(t, res.Errors, fixture)
	}
}

// The stop switch is set only while suspended: a running cluster renders as
// it did before suspending existed, so nothing restarts on upgrade.
func TestSuspendRendersStopSwitch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance *corev1alpha1.Instance
		stop     bool
	}{
		{"running", newReplicatedTestInstance(""), false},
		{"suspended", suspend(newReplicatedTestInstance("")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestContext(t, tc.instance)
			chi, err := buildCHI(c, replicasLayout(2))
			require.NoError(t, err)
			chk, err := buildCHK(c)
			require.NoError(t, err)

			if tc.stop {
				assert.True(t, chi.Spec.Stop.Value())
				assert.True(t, chk.Spec.Stop.Value())
			} else {
				assert.Nil(t, chi.Spec.Stop)
				assert.Nil(t, chk.Spec.Stop)
			}
		})
	}
}

// Suspending is the user's change: its inputs differ, while a running
// Instance keeps the inputs hash recorded before suspending existed.
func TestSuspendInputs(t *testing.T) {
	running := newTestInstance("db", "ns", "")
	got, err := inputsHash(newTestContext(t, running))
	require.NoError(t, err)
	before, err := hashJSON(struct {
		Version     string `json:"version,omitempty"`
		Topology    any    `json:"topology,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
		Components  any    `json:"components,omitempty"`
		Credentials string `json:"credentials,omitempty"`
	}{running.Spec.Version, running.Spec.Topology, running.Spec.Parameters, running.Spec.Components, ""})
	require.NoError(t, err)
	assert.Equal(t, before, got)

	suspendedHash, err := inputsHash(newTestContext(t, suspend(running)))
	require.NoError(t, err)
	assert.NotEqual(t, got, suspendedHash)
}

// Suspending applies the stop switch right away, without maintenance
// approval, which the runtime would hold while the Instance is suspended.
func TestSuspendIsNotHeld(t *testing.T) {
	running := newTestInstance("db", "ns", "")
	state := recordedState(t, running)

	applied, c := syncStandalone(t, suspend(running), completedCHI(), state)

	require.Len(t, applied, 1)
	assert.True(t, applied[0].Spec.Stop.Value())
	assert.Empty(t, c.GetPendingMaintenance())

	// Resuming drops the switch, again without approval.
	resumed := recordedState(t, suspend(running))
	applied, c = syncStandalone(t, running, completedCHI(), resumed)
	require.Len(t, applied, 1)
	assert.Nil(t, applied[0].Spec.Stop)
	assert.Empty(t, c.GetPendingMaintenance())
}

// ClickHouse stops before Keeper, whose quorum its replicated tables need
// while they run; on resume Keeper starts first.
func TestSuspendOrder(t *testing.T) {
	kinds, c := syncReplicated(t, suspend(newReplicatedTestInstance("")), completedCHK(), completedCHI())
	assert.Equal(t, []string{"ClickHouseInstallation", "ClickHouseKeeperInstallation"}, kinds)
	assert.Empty(t, c.GetPendingMaintenance())

	kinds, _ = syncReplicated(t, newReplicatedTestInstance(""), completedCHK(), completedCHI())
	assert.Equal(t, []string{"ClickHouseKeeperInstallation", "ClickHouseInstallation"}, kinds)
}

// stoppedCHI and stoppedCHK are installations the operator reconciled with
// their stop switch on.
func stoppedCHI(s operatorState) *chiv1.ClickHouseInstallation {
	chi := chiIn(s)
	chi.Spec.Stop = chtypes.NewStringBool(true)
	return chi
}

func stoppedCHK(s operatorState) *chkv1.ClickHouseKeeperInstallation {
	chk := chkIn(s)
	chk.Spec.Stop = chtypes.NewStringBool(true)
	return chk
}

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels}}
}

func TestStatusWhileSuspended(t *testing.T) {
	standalone := suspend(newTestInstance("db", "ns", ""))
	replicated := suspend(newReplicatedTestInstance(""))
	clickhousePod := pod("chi-db-clickhouse-0-0-0", map[string]string{common.LabelCHIName: "db"})
	keeperPod := pod("chk-db-keeper-keeper-0-0-0", map[string]string{common.LabelCHKName: "db-keeper"})

	for _, tc := range []struct {
		name     string
		instance *corev1alpha1.Instance
		live     []client.Object
		phase    corev1alpha1.InstancePhase
		message  string
	}{
		{"nothing created yet", standalone, nil,
			corev1alpha1.InstancePhaseSuspending, "Creating ClickHouse"},
		{"stop switch not applied yet", standalone, []client.Object{chiIn(completed)},
			corev1alpha1.InstancePhaseSuspending, "Stopping ClickHouse"},
		{"operator stopping", standalone, []client.Object{stoppedCHI(updating)},
			corev1alpha1.InstancePhaseSuspending, "Stopping ClickHouse"},
		{"pod still terminating", standalone, []client.Object{stoppedCHI(completed), clickhousePod},
			corev1alpha1.InstancePhaseSuspending, "Stopping ClickHouse: 1 pods left"},
		{"stopped", standalone, []client.Object{stoppedCHI(completed)},
			corev1alpha1.InstancePhaseSuspended, ""},

		{"replicated: ClickHouse still running", replicated, []client.Object{chkIn(completed), chiIn(completed)},
			corev1alpha1.InstancePhaseSuspending, "Stopping ClickHouse"},
		{"replicated: Keeper still running", replicated, []client.Object{chkIn(completed), stoppedCHI(completed)},
			corev1alpha1.InstancePhaseSuspending, "Stopping ClickHouse Keeper"},
		{"replicated: Keeper pod still terminating", replicated, []client.Object{stoppedCHK(completed), stoppedCHI(completed), keeperPod},
			corev1alpha1.InstancePhaseSuspending, "Stopping ClickHouse Keeper: 1 pods left"},
		{"replicated: stopped", replicated, []client.Object{stoppedCHK(completed), stoppedCHI(completed)},
			corev1alpha1.InstancePhaseSuspended, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := New().Status(newTestContext(t, tc.instance.DeepCopy(), tc.live...))
			require.NoError(t, err)
			assert.Equal(t, tc.phase, st.Phase)
			assert.Equal(t, tc.message, st.Message)
			assert.True(t, st.ConnectionDetails.IsEmpty(), "a suspended Instance has no client Service")
		})
	}
}
