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
	"strings"
	"testing"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

func int32Ptr(i int32) *int32 {
	return &i
}

func testInstanceWithSchedules(name, ns, uid string, enabled bool, schedules ...corev1alpha1.InstanceBackupSchedule) *corev1alpha1.Instance {
	inst := newTestInstance(name, ns, "")
	inst.UID = types.UID(uid)
	inst.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
		Enabled: enabled,
		Storages: []corev1alpha1.InstanceBackupStorage{
			{
				StorageRef: commonv1alpha1.ObjectRef{Name: "s3-backup"},
				Schedules:  schedules,
			},
		},
	}
	return inst
}

func TestScheduleCRName(t *testing.T) {
	assert.Equal(t, "my-db-daily", scheduleCRName("my-db", "daily"))
}

func TestSyncBackupSchedules_TwoSchedules(t *testing.T) {
	seven := int32(7)
	schedules := []corev1alpha1.InstanceBackupSchedule{
		{
			Name:       "daily",
			Enabled:    true,
			Cron:       "0 2 * * *",
			Retention:  &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount, Count: &seven},
			Parameters: &runtime.RawExtension{Raw: []byte(`{"replicaSelection":"FirstPerShard"}`)},
		},
		{
			Name:       "weekly",
			Enabled:    false,
			Cron:       "0 3 * * 0",
			Retention:  nil,
			Parameters: nil,
		},
	}

	inst := testInstanceWithSchedules("db", "ns", "db-uid-1234", true, schedules...)
	c := newTestContext(t, inst)

	err := syncBackupSchedules(c)
	require.NoError(t, err)

	// Verify daily CHBS
	dailyCHBS := &chiv1.ClickHouseBackupSchedule{}
	err = c.Get(dailyCHBS, "db-daily")
	require.NoError(t, err)
	assert.Equal(t, "db", dailyCHBS.Spec.ClickHouseInstallation)
	assert.Equal(t, "0 2 * * *", dailyCHBS.Spec.Schedule)
	require.NotNil(t, dailyCHBS.Spec.Suspend)
	assert.False(t, dailyCHBS.Spec.IsSuspended())
	require.NotNil(t, dailyCHBS.Spec.Retention.KeepLast)
	assert.Equal(t, int32(7), *dailyCHBS.Spec.Retention.KeepLast)
	assert.Equal(t, chiv1.ReplicaSelectionFirstPerShard, dailyCHBS.Spec.ReplicaSelection)
	assert.Empty(t, dailyCHBS.Spec.BackupNamePrefix)

	owner := metav1.GetControllerOf(dailyCHBS)
	require.NotNil(t, owner)
	assert.Equal(t, inst.Name, owner.Name)
	assert.Equal(t, inst.UID, owner.UID)

	// Verify weekly CHBS
	weeklyCHBS := &chiv1.ClickHouseBackupSchedule{}
	err = c.Get(weeklyCHBS, "db-weekly")
	require.NoError(t, err)
	assert.Equal(t, "db", weeklyCHBS.Spec.ClickHouseInstallation)
	assert.Equal(t, "0 3 * * 0", weeklyCHBS.Spec.Schedule)
	require.NotNil(t, weeklyCHBS.Spec.Suspend)
	assert.True(t, weeklyCHBS.Spec.IsSuspended())
	assert.Nil(t, weeklyCHBS.Spec.Retention.KeepLast)
	assert.Empty(t, weeklyCHBS.Spec.ReplicaSelection)
	assert.Empty(t, weeklyCHBS.Spec.BackupNamePrefix)

	owner = metav1.GetControllerOf(weeklyCHBS)
	require.NotNil(t, owner)
	assert.Equal(t, inst.Name, owner.Name)
	assert.Equal(t, inst.UID, owner.UID)
}

func TestSyncBackupSchedules_RemovalAndDisable(t *testing.T) {
	inst1 := testInstanceWithSchedules("db1", "ns", "db1-uid", true,
		corev1alpha1.InstanceBackupSchedule{Name: "daily", Enabled: true, Cron: "0 2 * * *"},
		corev1alpha1.InstanceBackupSchedule{Name: "weekly", Enabled: true, Cron: "0 3 * * 0"},
	)
	inst2 := testInstanceWithSchedules("db2", "ns", "db2-uid", true,
		corev1alpha1.InstanceBackupSchedule{Name: "daily", Enabled: true, Cron: "0 4 * * *"},
	)

	// Other CHBS owned by inst2
	otherCHBS := &chiv1.ClickHouseBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db2-daily",
			Namespace: "ns",
		},
		Spec: chiv1.ClickHouseBackupScheduleSpec{
			ClickHouseInstallation: "db2",
			Schedule:               "0 4 * * *",
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, chiv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(inst2, otherCHBS, scheme))

	c := newTestContext(t, inst1, otherCHBS)

	// 1. Initial sync creates db1-daily and db1-weekly
	require.NoError(t, syncBackupSchedules(c))

	var list chiv1.ClickHouseBackupScheduleList
	require.NoError(t, c.List(&list))
	assert.Len(t, list.Items, 3)

	// 2. Remove "weekly" schedule from db1
	inst1.Spec.Backup.Storages[0].Schedules = []corev1alpha1.InstanceBackupSchedule{
		{Name: "daily", Enabled: true, Cron: "0 2 * * *"},
	}
	require.NoError(t, syncBackupSchedules(c))

	require.NoError(t, c.List(&list))
	require.Len(t, list.Items, 2)
	names := []string{list.Items[0].Name, list.Items[1].Name}
	assert.Contains(t, names, "db1-daily")
	assert.Contains(t, names, "db2-daily")
	assert.NotContains(t, names, "db1-weekly")

	// 3. Disable backup on db1
	inst1.Spec.Backup.Enabled = false
	require.NoError(t, syncBackupSchedules(c))

	require.NoError(t, c.List(&list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, "db2-daily", list.Items[0].Name)
}

func TestValidateBackupSchedules(t *testing.T) {
	validDaily := &corev1alpha1.InstanceBackupSpec{
		Enabled: true,
		Storages: []corev1alpha1.InstanceBackupStorage{
			{
				StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
				Schedules: []corev1alpha1.InstanceBackupSchedule{
					{
						Name:    "daily",
						Enabled: true,
						Cron:    "0 2 * * *",
						Retention: &corev1alpha1.BackupScheduleRetention{
							Type:  corev1alpha1.BackupScheduleRetentionTypeCount,
							Count: int32Ptr(5),
						},
						Parameters: &runtime.RawExtension{
							Raw: []byte(`{"replicaSelection":"FirstPerShard"}`),
						},
					},
				},
			},
		},
	}

	t.Run("valid schedule passes", func(t *testing.T) {
		assert.NoError(t, validateBackupSchedules(validDaily, "db"))
	})

	t.Run("valid descriptor cron passes", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Cron = "@daily"
		assert.NoError(t, validateBackupSchedules(spec, "db"))
	})

	t.Run("nil backup passes", func(t *testing.T) {
		assert.NoError(t, validateBackupSchedules(nil, "db"))
	})

	t.Run("disabled backup passes anything", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Enabled = false
		spec.Storages[0].Schedules[0].Cron = "6 fields cron expression * * *"
		spec.Storages[0].Schedules[0].Name = "INVALID_NAME_$$$"
		spec.Storages[0].Schedules[0].Retention = &corev1alpha1.BackupScheduleRetention{
			Type:     corev1alpha1.BackupScheduleRetentionTypeTime,
			Duration: "30d",
		}
		spec.Storages[0].Schedules[0].Parameters = &runtime.RawExtension{
			Raw: []byte(`{"replicaSelection":"Invalid"}`),
		}
		assert.NoError(t, validateBackupSchedules(spec, "db"))
	})

	t.Run("time retention rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Retention = &corev1alpha1.BackupScheduleRetention{
			Type:     corev1alpha1.BackupScheduleRetentionTypeTime,
			Duration: "30d",
		}
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the clickhouse-backup operator only supports count retention (keepLast); use type count")
	})

	t.Run("6-field cron rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Cron = "0 0 2 * * *"
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must have exactly 5 space-separated fields or start with '@'")
	})

	t.Run("empty cron rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Cron = ""
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must have exactly 5 space-separated fields or start with '@'")
	})

	t.Run("invalid name rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Name = "Daily_Backup"
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not a valid DNS-1123 label")
	})

	t.Run("too-long combined name rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		// Total length db (2) + "-" (1) + 61 = 64 > 63
		spec.Storages[0].Schedules[0].Name = strings.Repeat("a", 61)
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not a valid DNS-1123 label")
	})

	t.Run("invalid replicaSelection rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Parameters = &runtime.RawExtension{
			Raw: []byte(`{"replicaSelection":"InvalidMode"}`),
		}
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid replicaSelection \"InvalidMode\"")
	})

	t.Run("malformed parameters json rejected", func(t *testing.T) {
		spec := validDaily.DeepCopy()
		spec.Storages[0].Schedules[0].Parameters = &runtime.RawExtension{
			Raw: []byte(`{"replicaSelection":`),
		}
		err := validateBackupSchedules(spec, "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid backup parameters")
	})
}

func TestProviderWiring_WatchOwnedCHBS(t *testing.T) {
	p := New()
	found := false
	for _, w := range p.WatchConfigs {
		if _, ok := w.Object.(*chiv1.ClickHouseBackupSchedule); ok && w.Owned {
			found = true
			break
		}
	}
	assert.True(t, found, "expected ClickHouseBackupSchedule in WatchConfigs")
}

func TestProviderWiring_Validate(t *testing.T) {
	p := New()
	prov := testProvider(t)
	inst := testInstanceWithSchedules("db", "ns", "db-uid", true,
		corev1alpha1.InstanceBackupSchedule{
			Name: "invalid_name",
			Cron: "0 2 * * *",
		},
	)
	res := p.Validate(controller.ValidationInput{Instance: inst, Provider: prov})
	require.NotEmpty(t, res.Errors)
	assert.Contains(t, res.Errors.ToAggregate().Error(), "is not a valid DNS-1123 label")

	inst.Spec.Backup.Storages[0].Schedules[0].Name = "valid-daily"
	res = p.Validate(controller.ValidationInput{Instance: inst, Provider: prov})
	assert.Empty(t, res.Errors)
}

func TestProviderWiring_SyncAfterTopology(t *testing.T) {
	p := New()
	inst := testInstanceWithSchedules("db", "ns", "db-uid", true,
		corev1alpha1.InstanceBackupSchedule{
			Name:    "daily",
			Enabled: true,
			Cron:    "0 2 * * *",
		},
	)

	bs := testBackupStorage("s3-backup", "s3-creds", "bucket", "https://s3.example.com", "us-east-1", nil, nil)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "ns"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("access-key"),
			"AWS_SECRET_ACCESS_KEY": []byte("secret-key"),
		},
	}

	// 1. Without completed CHI: sync returns a WaitError, CHBS is NOT created
	var applied []*chiv1.ClickHouseInstallation
	cWaiting := newInterceptedTestContext(t, captureCHIApply(t, &applied), inst, withCredentials([]client.Object{bs, creds})...)
	err := p.Sync(cWaiting)
	require.Error(t, err)
	assert.True(t, controller.IsWaitError(err))

	chbs := &chiv1.ClickHouseBackupSchedule{}
	err = cWaiting.Get(chbs, "db-daily")
	assert.Error(t, err, "CHBS should not be created while CHI is still reconciling")

	// 2. With completed CHI: sync succeeds, CHBS IS created
	chi := completedCHI()
	cCompleted := newInterceptedTestContext(t, captureCHIApply(t, &applied), inst, withCredentials([]client.Object{bs, creds, chi})...)
	err = p.Sync(cCompleted)
	require.NoError(t, err)

	err = cCompleted.Get(chbs, "db-daily")
	require.NoError(t, err, "CHBS should be created once CHI is completed")
	assert.Equal(t, "db", chbs.Spec.ClickHouseInstallation)
	assert.Equal(t, "0 2 * * *", chbs.Spec.Schedule)
}

