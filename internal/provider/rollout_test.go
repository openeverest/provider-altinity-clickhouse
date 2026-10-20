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
	"context"
	"encoding/json"
	"strings"
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// withCredentials adds the Instance's credentials Secret to live objects
// unless they carry one: it persists across reconciles in a real cluster, and
// a regenerated password would look like the user rotating it.
func withCredentials(live []client.Object) []client.Object {
	for _, obj := range live {
		if obj.GetName() == credentialsSecretName("db") {
			return live
		}
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName("db"), Namespace: "ns"},
		Data: map[string][]byte{
			"username":             []byte("admin"),
			"password":             []byte("test-password-0123456789"),
			"default_password":     []byte("test-default-password"),
			"interserver_password": []byte("test-interserver-password"),
			"cluster_secret":       []byte("test-cluster-secret"),
		},
	}
	return append(append([]client.Object{}, live...), creds)
}

// completedCHI is a CHI the operator has finished reconciling.
func completedCHI() *chiv1.ClickHouseInstallation {
	chi := &chiv1.ClickHouseInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	chi.EnsureStatus().Status = chiv1.StatusCompleted
	return chi
}

// syncStandalone runs Sync for instance against live objects and returns the
// CHIs it applied and the context (for pending maintenance and state).
func syncStandalone(t *testing.T, instance *corev1alpha1.Instance, live ...client.Object) ([]*chiv1.ClickHouseInstallation, *controller.Context) {
	t.Helper()
	var applied []*chiv1.ClickHouseInstallation
	c := newInterceptedTestContext(t, captureCHIApply(t, &applied), instance, withCredentials(live)...)
	err := New().Sync(c)
	if err != nil {
		require.True(t, controller.IsWaitError(err), "unexpected error: %v", err)
	}
	return applied, c
}

// rolloutState reads back the recorded state Secret, if any.
func rolloutState(t *testing.T, c *controller.Context) *corev1.Secret {
	t.Helper()
	state := &corev1.Secret{}
	if err := c.Get(state, rolloutStateName("db")); err != nil {
		return nil
	}
	return state
}

// recordedState runs one Sync of instance on a Completed CHI and returns the
// state Secret it records, as the starting point of a later reconcile.
func recordedState(t *testing.T, instance *corev1alpha1.Instance) *corev1.Secret {
	t.Helper()
	_, c := syncStandalone(t, instance, completedCHI())
	state := rolloutState(t, c)
	require.NotNil(t, state)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: state.Name, Namespace: state.Namespace},
		Data:       state.Data,
	}
}

// providerChanged returns state as if an earlier provider version had rendered
// a different CHI from the same Instance.
func providerChanged(state *corev1.Secret) *corev1.Secret {
	changed := state.DeepCopy()
	changed.Data["chi.spec"] = []byte("rendered-by-an-earlier-provider")
	return changed
}

func TestRolloutAppliesNewCluster(t *testing.T) {
	applied, c := syncStandalone(t, newTestInstance("db", "ns", ""))

	assert.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
	require.NotNil(t, rolloutState(t, c))
}

func TestRolloutBaselinesClusterWithoutRecord(t *testing.T) {
	// Last applied by a provider predating the gate: applied and recorded.
	applied, c := syncStandalone(t, newTestInstance("db", "ns", ""), completedCHI())

	assert.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
	state := rolloutState(t, c)
	require.NotNil(t, state)
	assert.NotEmpty(t, state.Data["chi.spec"])
	assert.NotEmpty(t, state.Data["chi.inputs-v3"])
}

func TestRolloutAppliesUnchangedRendering(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	applied, c := syncStandalone(t, instance, completedCHI(), recordedState(t, instance))

	assert.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
}

func TestRolloutHoldsProviderInitiatedChange(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := providerChanged(recordedState(t, instance))

	applied, c := syncStandalone(t, instance, completedCHI(), state)

	assert.Empty(t, applied, "a provider-initiated change must wait for approval")
	pending := c.GetPendingMaintenance()
	require.Len(t, pending, 1)
	assert.Equal(t, controller.MaintenanceRollingRestart, pending[0].Severity)
	assert.True(t, strings.HasPrefix(pending[0].ApprovalToken, "provider-0.1.0-"), pending[0].ApprovalToken)
	// Nothing was applied, so the record still describes the deployed CHI.
	assert.Equal(t, state.Data["chi.spec"], rolloutState(t, c).Data["chi.spec"])
}

func TestRolloutAppliesProviderChangeWithinTolerance(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := providerChanged(recordedState(t, instance))
	instance.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{AutoApproveUpTo: controller.MaintenanceRollingRestart}

	applied, c := syncStandalone(t, instance, completedCHI(), state)

	assert.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
	assert.NotEqual(t, state.Data["chi.spec"], rolloutState(t, c).Data["chi.spec"])
}

func TestRolloutAppliesApprovedProviderChange(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := providerChanged(recordedState(t, instance))
	_, held := syncStandalone(t, instance, completedCHI(), state)
	require.Len(t, held.GetPendingMaintenance(), 1)

	instance.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: held.GetPendingMaintenance()[0].ApprovalToken}
	applied, c := syncStandalone(t, instance, completedCHI(), state)

	assert.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
}

func TestRolloutAppliesUserChangeImmediately(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := providerChanged(recordedState(t, instance))

	// The user edits the Instance: the pending provider change rides along.
	edited := newTestInstance("db", "ns", `{"configuration":"max_concurrent_queries: 200"}`)
	applied, c := syncStandalone(t, edited, completedCHI(), state)

	assert.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
}

func TestRolloutIgnoresMaintenanceOnlyEdits(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := providerChanged(recordedState(t, instance))

	// Changing only the maintenance tolerance is not a change to the cluster.
	instance.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{AutoApproveUpTo: controller.MaintenanceNonDisruptive}
	applied, c := syncStandalone(t, instance, completedCHI(), state)

	assert.Empty(t, applied)
	assert.Len(t, c.GetPendingMaintenance(), 1)
}

// captureAppliedKinds records the Kinds of applied ClickHouse and Keeper
// installations instead of forwarding them (the fake client cannot apply
// them); everything else is applied.
func captureAppliedKinds(t *testing.T, kinds *[]string) interceptor.Funcs {
	t.Helper()
	return interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			data, err := json.Marshal(obj)
			require.NoError(t, err)
			var meta struct {
				Kind string `json:"kind"`
			}
			require.NoError(t, json.Unmarshal(data, &meta))
			if meta.Kind == "ClickHouseInstallation" || meta.Kind == "ClickHouseKeeperInstallation" {
				*kinds = append(*kinds, meta.Kind)
				return nil
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
}

func completedCHK() *chkv1.ClickHouseKeeperInstallation {
	chk := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db-keeper", Namespace: "ns"}}
	chk.EnsureStatus().Status = chkv1.StatusCompleted
	return chk
}

func syncReplicated(t *testing.T, instance *corev1alpha1.Instance, live ...client.Object) ([]string, *controller.Context) {
	t.Helper()
	var kinds []string
	c := newInterceptedTestContext(t, captureAppliedKinds(t, &kinds), instance, withCredentials(live)...)
	if err := New().Sync(c); err != nil {
		require.True(t, controller.IsWaitError(err), "unexpected error: %v", err)
	}
	return kinds, c
}

func TestRolloutReplicatedHeldAsOneAction(t *testing.T) {
	instance := newReplicatedTestInstance("")
	_, first := syncReplicated(t, instance, completedCHK(), completedCHI())
	recorded := rolloutState(t, first)
	require.NotNil(t, recorded)
	require.NotEmpty(t, recorded.Data["chk.spec"])

	state := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: recorded.Name, Namespace: recorded.Namespace}, Data: recorded.Data}
	state = providerChanged(state)
	state.Data["chk.spec"] = []byte("rendered-by-an-earlier-provider")

	kinds, c := syncReplicated(t, instance, completedCHK(), completedCHI(), state)
	assert.Empty(t, kinds, "neither Keeper nor ClickHouse may roll before approval")
	assert.Len(t, c.GetPendingMaintenance(), 1, "Keeper and ClickHouse are one held action")

	instance.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: c.GetPendingMaintenance()[0].ApprovalToken}
	kinds, c = syncReplicated(t, instance, completedCHK(), completedCHI(), state)
	assert.Equal(t, []string{"ClickHouseKeeperInstallation", "ClickHouseInstallation"}, kinds)
	assert.Empty(t, c.GetPendingMaintenance())
}

func TestRolloutPublishesPendingMaintenanceWhileWaiting(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := providerChanged(recordedState(t, instance))
	rolling := completedCHI()
	rolling.EnsureStatus().Status = chiv1.StatusInProgress

	// Held while the operator is still busy: Sync waits, yet the hold is shown.
	_, c := syncStandalone(t, instance, rolling, state)
	live := &corev1alpha1.Instance{}
	require.NoError(t, c.Get(live, "db"))
	require.Len(t, live.Status.PendingMaintenance, 1)
	assert.True(t, meta.IsStatusConditionTrue(live.Status.Conditions, corev1alpha1.ConditionMaintenancePending))

	// Approved: the rollout starts and Sync waits on it; the hold is cleared
	// at once rather than when the cluster is Ready again.
	live.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: live.Status.PendingMaintenance[0].ApprovalToken}
	_, c = syncStandalone(t, live, rolling, state)
	require.NoError(t, c.Get(live, "db"))
	assert.Empty(t, live.Status.PendingMaintenance)
	assert.True(t, meta.IsStatusConditionFalse(live.Status.Conditions, corev1alpha1.ConditionMaintenancePending))
}

func TestCleanupRemovesRolloutState(t *testing.T) {
	state := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rolloutStateName("db"), Namespace: "ns"}}
	c := newTestContext(t, newTestInstance("db", "ns", ""), state)

	require.NoError(t, New().Cleanup(c))
	assert.Nil(t, rolloutState(t, c))
}

// prepared returns the copy of stored the runtime hands Sync: component
// versions filled in from the version bundle.
func prepared(stored *corev1alpha1.Instance, engineVersion string) *corev1alpha1.Instance {
	return withEngine(stored, func(e *corev1alpha1.ComponentSpec) { e.Version = engineVersion })
}

// syncPrepared runs Sync on the prepared copy of stored, with stored in the
// API, and returns the CHIs applied and the context.
func syncPrepared(t *testing.T, stored, preparedIn *corev1alpha1.Instance, live ...client.Object) ([]*chiv1.ClickHouseInstallation, *controller.Context) {
	t.Helper()
	var applied []*chiv1.ClickHouseInstallation
	base := newInterceptedTestContext(t, captureCHIApply(t, &applied), stored, withCredentials(live)...)
	c := controller.NewContext(context.Background(), base.Client(), preparedIn, "altinity-clickhouse")
	if err := New().Sync(c); err != nil {
		require.True(t, controller.IsWaitError(err), "unexpected error: %v", err)
	}
	return applied, c
}

// A provider release that moves a version bundle to a new patch changes the
// versions the runtime fills in, not the Instance: it must be held.
func TestRolloutHoldsBundleMovedByProvider(t *testing.T) {
	stored := withEngine(newTestInstance("db", "ns", ""), image(""))

	_, first := syncPrepared(t, stored, prepared(stored, "26.3.56.2"), completedCHI())
	recorded := rolloutState(t, first)
	require.NotNil(t, recorded)
	state := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: recorded.Name, Namespace: recorded.Namespace}, Data: recorded.Data}

	applied, c := syncPrepared(t, stored, prepared(stored, "26.8.31.2"), completedCHI(), state)
	assert.Empty(t, applied)
	assert.Len(t, c.GetPendingMaintenance(), 1)
}

// Records of the earlier format fingerprinted the prepared Instance under
// <cr>.inputs: a changed rendering is held, and approving it records the
// current format (the stale key is ignored, and dropped by server-side apply).
func TestRolloutHoldsChangeOnLegacyRecord(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: rolloutStateName("db"), Namespace: "ns"},
		Data: map[string][]byte{
			"chi.spec":   []byte("rendered-by-an-earlier-provider"),
			"chi.inputs": []byte("fingerprint-of-the-prepared-instance"),
		},
	}

	applied, c := syncStandalone(t, instance, completedCHI(), legacy)
	assert.Empty(t, applied)
	require.Len(t, c.GetPendingMaintenance(), 1)

	instance.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: c.GetPendingMaintenance()[0].ApprovalToken}
	applied, c = syncStandalone(t, instance, completedCHI(), legacy)
	assert.Len(t, applied, 1)
	state := rolloutState(t, c)
	assert.NotEmpty(t, state.Data["chi.inputs-v3"])
}

// A record of the previous format (<cr>.instance, before credentials were
// fingerprinted) holds a changed rendering instead of applying it.
func TestRolloutHoldsChangeOnPreviousFormatRecord(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	previous := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: rolloutStateName("db"), Namespace: "ns"},
		Data: map[string][]byte{
			"chi.spec":     []byte("rendered-by-an-earlier-provider"),
			"chi.instance": []byte("fingerprint-without-credentials"),
		},
	}

	applied, c := syncStandalone(t, instance, completedCHI(), previous)
	assert.Empty(t, applied)
	assert.Len(t, c.GetPendingMaintenance(), 1)
}
