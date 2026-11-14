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

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	apicommon "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

const testStorage = "s3-backups"

// withBackups returns a copy of instance keeping its backups in testStorage,
// with the schedules given and scheduled backups enabled.
func withBackups(instance *corev1alpha1.Instance, schedules ...corev1alpha1.InstanceBackupSchedule) *corev1alpha1.Instance {
	out := instance.DeepCopy()
	out.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
		Enabled:  true,
		ClassRef: apicommon.ObjectRef{Name: "altinity-clickhouse-backup"},
		Storages: []corev1alpha1.InstanceBackupStorage{{
			StorageRef: apicommon.ObjectRef{Name: testStorage},
			Schedules:  schedules,
		}},
	}
	return out
}

// s3Storage is a BackupStorage and its credentials Secret.
func s3Storage() []client.Object {
	return []client.Object{
		&backupv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: testStorage, Namespace: "ns"},
			Spec: backupv1alpha1.BackupStorageSpec{
				Type: backupv1alpha1.BackupStorageTypeS3,
				S3: &backupv1alpha1.BackupStorageS3Spec{
					Bucket:               "clickhouse",
					Region:               "us-east-1",
					EndpointURL:          "https://seaweedfs.seaweedfs.svc",
					ForcePathStyle:       ptr.To(true),
					VerifyTLS:            ptr.To(false),
					CredentialsSecretRef: apicommon.SecretRef{Name: "s3-credentials"},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s3-credentials", Namespace: "ns"},
			Data: map[string][]byte{
				"AWS_ACCESS_KEY_ID":     []byte("key-id"),
				"AWS_SECRET_ACCESS_KEY": []byte("secret-key"),
			},
		},
	}
}

// The sidecar is configured only for an Instance with a backup storage, so
// every other Instance renders as before and keeps its recorded inputs.
func TestBackupSidecarRendering(t *testing.T) {
	plain := newTestInstance("db", "ns", "")
	c := newTestContext(t, plain)
	chi, err := buildCHI(c, replicasLayout(1))
	require.NoError(t, err)
	assert.Nil(t, chi.Spec.Backup)
	before, err := inputsHash(c)
	require.NoError(t, err)
	legacy, err := hashJSON(struct {
		Version     string `json:"version,omitempty"`
		Topology    any    `json:"topology,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
		Components  any    `json:"components,omitempty"`
		Credentials string `json:"credentials,omitempty"`
	}{plain.Spec.Version, plain.Spec.Topology, plain.Spec.Parameters, plain.Spec.Components, ""})
	require.NoError(t, err)
	assert.Equal(t, legacy, before)

	c = newTestContext(t, withBackups(plain))
	chi, err = buildCHI(c, replicasLayout(1))
	require.NoError(t, err)
	require.NotNil(t, chi.Spec.Backup)
	assert.True(t, chi.Spec.Backup.IsEnabled())
	assert.Equal(t, "db-backup-storage", chi.Spec.Backup.ConfigSecretRef.Name)
	after, err := inputsHash(c)
	require.NoError(t, err)
	assert.NotEqual(t, before, after, "adding a storage is the user's change, applied without approval")
}

func TestReconcileBackupConfig(t *testing.T) {
	c := newTestContext(t, withBackups(newTestInstance("db", "ns", "")), s3Storage()...)
	require.NoError(t, reconcileBackupConfig(c))

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(secret, "db-backup-storage"))
	var config map[string]map[string]any
	require.NoError(t, yaml.Unmarshal(secret.Data[common.BackupConfigKey], &config))
	assert.Equal(t, map[string]any{"remote_storage": "s3"}, config["general"])
	assert.Equal(t, map[string]any{
		"endpoint":                  "https://seaweedfs.seaweedfs.svc",
		"region":                    "us-east-1",
		"bucket":                    "clickhouse",
		"access_key":                "key-id",
		"secret_key":                "secret-key",
		"force_path_style":          true,
		"disable_cert_verification": true,
	}, config["s3"])

	// Without a storage the Secret goes.
	c = newTestContext(t, newTestInstance("db", "ns", ""), secret)
	require.NoError(t, reconcileBackupConfig(c))
	found, err := getOptional(c, &corev1.Secret{}, "db-backup-storage")
	require.NoError(t, err)
	assert.False(t, found)
}

func coreBackup(name, storage, params string) *backupv1alpha1.Backup {
	b := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: "backup-uid"},
		Spec: backupv1alpha1.BackupSpec{
			Origin:     backupv1alpha1.BackupOrigin{Type: backupv1alpha1.BackupOriginTypeInstance, InstanceRef: &apicommon.ObjectRef{Name: "db"}},
			ClassRef:   apicommon.ObjectRef{Name: "altinity-clickhouse-backup"},
			StorageRef: apicommon.ObjectRef{Name: storage},
		},
	}
	if params != "" {
		b.Spec.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}
	return b
}

// backupReadyCHI is a CHI the operator reconciled with the backup sidecar on.
func backupReadyCHI() *chiv1.ClickHouseInstallation {
	chi := completedCHI()
	chi.Spec.Backup = &chiv1.ClusterBackup{Enabled: chiv1NewTrue(), ConfigSecretRef: &corev1.LocalObjectReference{Name: "db-backup-storage"}}
	return chi
}

func TestSyncBackup(t *testing.T) {
	instance := withBackups(newTestInstance("db", "ns", ""))

	t.Run("no storage on the Instance", func(t *testing.T) {
		st, err := New().SyncBackup(newTestContext(t, newTestInstance("db", "ns", "")), coreBackup("b1", testStorage, ""))
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, st.State)
	})
	t.Run("another storage", func(t *testing.T) {
		st, err := New().SyncBackup(newTestContext(t, instance), coreBackup("b1", "elsewhere", ""))
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, st.State)
		assert.Contains(t, st.Message, testStorage)
	})
	t.Run("sidecar not rolled out yet", func(t *testing.T) {
		c := newTestContext(t, instance, completedCHI())
		st, err := New().SyncBackup(c, coreBackup("b1", testStorage, ""))
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStatePending, st.State)
		found, err := getOptional(c, &chiv1.ClickHouseBackup{}, "b1")
		require.NoError(t, err)
		assert.False(t, found, "no backup before the sidecar runs")
	})
	t.Run("creates the operator backup", func(t *testing.T) {
		backup := coreBackup("b1", testStorage, `{"replicaSelection":"AllReplicas"}`)
		c := newTestContext(t, instance, backupReadyCHI(), backup)
		st, err := New().SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStatePending, st.State)
		require.NotNil(t, st.OperatorBackupRef)
		assert.Equal(t, "ClickHouseBackup", st.OperatorBackupRef.Kind)

		chb := &chiv1.ClickHouseBackup{}
		require.NoError(t, c.Get(chb, "b1"))
		assert.Equal(t, "db", chb.Spec.ClickHouseInstallation)
		assert.Equal(t, chiv1.ReplicaSelection("AllReplicas"), chb.Spec.ReplicaSelection)
		require.Len(t, chb.OwnerReferences, 1)
		assert.Equal(t, "Backup", chb.OwnerReferences[0].Kind)
		assert.True(t, *chb.OwnerReferences[0].Controller)
	})

	for _, tc := range []struct {
		phase   chiv1.OperationPhase
		state   backupv1alpha1.BackupState
		message string
	}{
		{chiv1.OperationPhasePending, backupv1alpha1.BackupStatePending, ""},
		{chiv1.OperationPhaseRunning, backupv1alpha1.BackupStateRunning, ""},
		{chiv1.OperationPhaseCompleted, backupv1alpha1.BackupStateSucceeded, "backup db-b1-20261113"},
		{chiv1.OperationPhaseFailed, backupv1alpha1.BackupStateFailed, "cluster clickhouse: upload failed"},
	} {
		t.Run("operator "+string(tc.phase), func(t *testing.T) {
			chb := &chiv1.ClickHouseBackup{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns"}}
			chb.Spec.ClickHouseInstallation = "db"
			chb.Status.Phase = tc.phase
			chb.Status.BackupName = "db-b1-20261113"
			chb.Status.Clusters = []chiv1.OperationClusterStatus{{Name: "clickhouse", Phase: tc.phase, Message: "upload failed"}}
			st, err := New().SyncBackup(newTestContext(t, instance, chb), coreBackup("b1", testStorage, ""))
			require.NoError(t, err)
			assert.Equal(t, tc.state, st.State)
			assert.Equal(t, tc.message, st.Message)
		})
	}
}

// succeededBackup is a core Backup of instance "source" and its operator backup.
func succeededBackup(storage string) []client.Object {
	b := coreBackup("b1", storage, "")
	b.Spec.Origin.InstanceRef = &apicommon.ObjectRef{Name: "source"}
	b.Status.State = backupv1alpha1.BackupStateSucceeded
	b.Status.OperatorBackupRef = &apicommon.TypedObjectRef{Kind: "ClickHouseBackup", Name: "b1"}
	chb := &chiv1.ClickHouseBackup{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns"}}
	chb.Status.Phase = chiv1.OperationPhaseCompleted
	chb.Status.BackupName = "source-b1-20261113"
	return []client.Object{b, chb}
}

func coreRestore(params string) *backupv1alpha1.Restore {
	r := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "db-datasource", Namespace: "ns", UID: "restore-uid"},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: apicommon.ObjectRef{Name: "db"},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: apicommon.ObjectRef{Name: "b1"}},
			},
		},
	}
	if params != "" {
		r.Spec.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}
	return r
}

func TestSyncRestore(t *testing.T) {
	instance := withBackups(newTestInstance("db", "ns", ""))

	t.Run("backup in another storage", func(t *testing.T) {
		c := newTestContext(t, instance, append(succeededBackup("elsewhere"), backupReadyCHI())...)
		st, err := New().SyncRestore(c, coreRestore(""))
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateFailed, st.State)
	})
	t.Run("restores another Instance's backup", func(t *testing.T) {
		restore := coreRestore(`{"overwrite":true}`)
		c := newTestContext(t, instance, append(succeededBackup(testStorage), backupReadyCHI(), restore)...)
		st, err := New().SyncRestore(c, restore)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStatePending, st.State)

		chr := &chiv1.ClickHouseRestore{}
		require.NoError(t, c.Get(chr, "db-datasource"))
		assert.Equal(t, "db", chr.Spec.ClickHouseInstallation)
		assert.Equal(t, "source-b1-20261113", chr.Spec.BackupName)
		assert.Equal(t, "source", chr.Spec.SourceInstallation)
		assert.True(t, chr.Spec.Overwrite.Value())
		require.Len(t, chr.OwnerReferences, 1)
		assert.Equal(t, "Restore", chr.OwnerReferences[0].Kind)
	})
	t.Run("operator completed", func(t *testing.T) {
		chr := &chiv1.ClickHouseRestore{ObjectMeta: metav1.ObjectMeta{Name: "db-datasource", Namespace: "ns"}}
		chr.Status.Phase = chiv1.OperationPhaseCompleted
		st, err := New().SyncRestore(newTestContext(t, instance, chr), coreRestore(""))
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.RestoreStateSucceeded, st.State)
	})
}

func TestDesiredBackupSchedules(t *testing.T) {
	nightly := corev1alpha1.InstanceBackupSchedule{Name: "nightly", Enabled: true, Cron: "0 2 * * *",
		Retention: &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount, Count: ptr.To[int32](7)}}
	hourly := corev1alpha1.InstanceBackupSchedule{Name: "hourly", Enabled: true, Cron: "0 * * * *",
		Retention: &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeTime, Duration: "24h"}}
	off := corev1alpha1.InstanceBackupSchedule{Name: "off", Cron: "0 3 * * *"}

	schedules := desiredBackupSchedules(newTestContext(t, withBackups(newTestInstance("db", "ns", ""), nightly, hourly, off)))
	require.Len(t, schedules, 2)
	assert.Equal(t, "db-nightly", schedules[0].Name)
	assert.Equal(t, "db", schedules[0].Spec.ClickHouseInstallation)
	assert.Equal(t, "0 2 * * *", schedules[0].Spec.Schedule)
	assert.Equal(t, ptr.To[int32](7), schedules[0].Spec.Retention.KeepLast)
	assert.Equal(t, "db-hourly", schedules[1].Name)
	assert.Nil(t, schedules[1].Spec.Retention.KeepLast, "the operator has no time-based retention")

	disabled := withBackups(newTestInstance("db", "ns", ""), nightly)
	disabled.Spec.Backup.Enabled = false
	assert.Empty(t, desiredBackupSchedules(newTestContext(t, disabled)))
}

func TestStatusWhileRestoringDataSource(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""), chiIn(completed))
	c.SetDataSourceStatus(controller.DataSourceStatus{Done: false, Message: "restore is running"})
	st, err := New().Status(c)
	require.NoError(t, err)
	assert.Equal(t, corev1alpha1.InstancePhaseRestoring, st.Phase)
	assert.Equal(t, "restore is running", st.Message)
}

func chiv1NewTrue() *chtypes.StringBool { return chtypes.NewStringBool(true) }
