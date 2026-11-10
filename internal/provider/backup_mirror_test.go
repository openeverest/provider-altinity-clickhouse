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
	"testing"
	"time"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/model/chbackup"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func newMirrorTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, chiv1.AddToScheme(scheme))
	return scheme
}

func newMirrorTestContext(t *testing.T, instance *corev1alpha1.Instance, interceptFuncs interceptor.Funcs, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	scheme := newMirrorTestScheme(t)

	all := append([]client.Object{instance, testProvider(t)}, objs...)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&chiv1.ClickHouseBackup{}, &backupv1alpha1.Backup{}, &corev1alpha1.Instance{}).
		WithInterceptorFuncs(interceptFuncs).
		Build()

	c := controller.NewContext(context.Background(), fakeClient, instance, common.ProviderName)
	return c, fakeClient
}

func testInstanceWithSchedule(name, ns, storageName, scheduleName string, retention *corev1alpha1.BackupScheduleRetention) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{
				Name: common.ProviderName,
			},
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled: true,
				ClassRef: commonv1alpha1.ObjectRef{
					Name: "ch-backup-class",
				},
				Storages: []corev1alpha1.InstanceBackupStorage{
					{
						StorageRef: commonv1alpha1.ObjectRef{
							Name: storageName,
						},
						Schedules: []corev1alpha1.InstanceBackupSchedule{
							{
								Name:      scheduleName,
								Enabled:   true,
								Cron:      "0 0 * * *",
								Retention: retention,
							},
						},
					},
				},
			},
		},
	}
}

// Acceptance criteria 1:
// A CHBS Job with slot 29741160 and the default prefix mirrors to a Backup named after the Job,
// with the right scheduleName, storageRef, classRef and annotation value (check the expected timestamp by hand).
func TestMirror_Success_Slot29741160(t *testing.T) {
	p := &Provider{}
	ns := "test-ns"
	instName := "my-instance"
	schedName := "daily"
	chbsName := instName + "-" + schedName
	cronJobName := chbsName + "-cluster1"
	slot := "29741160"
	jobName := cronJobName + "-" + slot

	instance := testInstanceWithSchedule(instName, ns, "s3-storage", schedName, nil)

	chbs := &chiv1.ClickHouseBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      chbsName,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: corev1alpha1.GroupVersion.String(),
					Kind:       "Instance",
					Name:       instName,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: chiv1.ClickHouseBackupScheduleSpec{
			ClickHouseInstallation: instName,
		},
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "CronJob",
					Name:       cronJobName,
					Controller: ptr.To(true),
				},
			},
			Labels: map[string]string{
				chbackup.LabelBackupCRKind: "ClickHouseBackupSchedule",
				chbackup.LabelBackupCRName: chbsName,
				chbackup.LabelBackupCHI:    instName,
			},
		},
	}

	c, fakeClient := newMirrorTestContext(t, instance, interceptor.Funcs{}, chbs, job)
	_ = c

	require.Equal(t, &batchv1.Job{}, p.OperatorBackupType())

	mirrored, err := p.Mirror(context.Background(), fakeClient, job)
	require.NoError(t, err)
	require.NotNil(t, mirrored)

	assert.Equal(t, jobName, mirrored.Name)
	assert.Equal(t, ns, mirrored.Namespace)
	assert.Equal(t, backupv1alpha1.BackupOriginTypeInstance, mirrored.Spec.Origin.Type)
	require.NotNil(t, mirrored.Spec.Origin.InstanceRef)
	assert.Equal(t, instName, mirrored.Spec.Origin.InstanceRef.Name)
	assert.Equal(t, "ch-backup-class", mirrored.Spec.ClassRef.Name)
	assert.Equal(t, "s3-storage", mirrored.Spec.StorageRef.Name)
	assert.Equal(t, schedName, mirrored.Spec.ScheduleName)

	// Hand-checked expected timestamp for slot 29741160:
	// 29741160 minutes * 60 = 1784469600 unix seconds.
	// 1784469600 in UTC = 2026-07-19 14:00:00 UTC.
	// Layout "20060102-150405" = "20260719-140000".
	// Default prefix = boundedName(joinName(ns, instName, chbsName)) = "test-ns-my-instance-my-instance-daily".
	expectedRemoteName := "test-ns-my-instance-my-instance-daily-20260719-140000"
	assert.Equal(t, expectedRemoteName, mirrored.Annotations[annotationRemoteBackupName])
}

// Acceptance criteria 2:
// Skipped (nil, nil):
// - a Job without the label
// - a ClickHouseBackup Job
// - a CHBS owned by another provider's Instance
// - an unknown schedule
// - a non-numeric slot
func TestMirror_Skipped(t *testing.T) {
	p := &Provider{}
	ns := "test-ns"
	instName := "my-instance"
	schedName := "daily"
	chbsName := instName + "-" + schedName
	cronJobName := chbsName + "-cluster1"

	baseInstance := testInstanceWithSchedule(instName, ns, "s3-storage", schedName, nil)

	baseCHBS := &chiv1.ClickHouseBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      chbsName,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: corev1alpha1.GroupVersion.String(),
					Kind:       "Instance",
					Name:       instName,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: chiv1.ClickHouseBackupScheduleSpec{
			ClickHouseInstallation: instName,
		},
	}

	tests := []struct {
		name     string
		instance *corev1alpha1.Instance
		chbs     *chiv1.ClickHouseBackupSchedule
		job      client.Object
	}{
		{
			name:     "non-Job object",
			instance: baseInstance,
			chbs:     baseCHBS,
			job:      &corev1.Pod{},
		},
		{
			name:     "Job without label",
			instance: baseInstance,
			chbs:     baseCHBS,
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cronJobName + "-12345",
					Namespace: ns,
				},
			},
		},
		{
			name:     "ClickHouseBackup Job",
			instance: baseInstance,
			chbs:     baseCHBS,
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cronJobName + "-12345",
					Namespace: ns,
					Labels: map[string]string{
						chbackup.LabelBackupCRKind: "ClickHouseBackup",
						chbackup.LabelBackupCRName: "my-ondemand-backup",
					},
				},
			},
		},
		{
			name: "CHBS owned by another provider's Instance",
			instance: func() *corev1alpha1.Instance {
				inst := baseInstance.DeepCopy()
				inst.Spec.ProviderRef.Name = "cloudnative-pg"
				return inst
			}(),
			chbs: baseCHBS,
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cronJobName + "-12345",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "batch/v1",
							Kind:       "CronJob",
							Name:       cronJobName,
							Controller: ptr.To(true),
						},
					},
					Labels: map[string]string{
						chbackup.LabelBackupCRKind: "ClickHouseBackupSchedule",
						chbackup.LabelBackupCRName: chbsName,
					},
				},
			},
		},
		{
			name: "unknown schedule",
			instance: func() *corev1alpha1.Instance {
				inst := baseInstance.DeepCopy()
				// Instance only has schedule "weekly", but CHBS is for "daily"
				inst.Spec.Backup.Storages[0].Schedules[0].Name = "weekly"
				return inst
			}(),
			chbs: baseCHBS,
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cronJobName + "-12345",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "batch/v1",
							Kind:       "CronJob",
							Name:       cronJobName,
							Controller: ptr.To(true),
						},
					},
					Labels: map[string]string{
						chbackup.LabelBackupCRKind: "ClickHouseBackupSchedule",
						chbackup.LabelBackupCRName: chbsName,
					},
				},
			},
		},
		{
			name:     "non-numeric slot",
			instance: baseInstance,
			chbs:     baseCHBS,
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cronJobName + "-notanumber",
					Namespace: ns,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "batch/v1",
							Kind:       "CronJob",
							Name:       cronJobName,
							Controller: ptr.To(true),
						},
					},
					Labels: map[string]string{
						chbackup.LabelBackupCRKind: "ClickHouseBackupSchedule",
						chbackup.LabelBackupCRName: chbsName,
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, fakeClient := newMirrorTestContext(t, tc.instance, interceptor.Funcs{}, tc.chbs)
			mirrored, err := p.Mirror(context.Background(), fakeClient, tc.job)
			require.NoError(t, err)
			assert.Nil(t, mirrored)
		})
	}
}

// Acceptance criteria 3:
// Job states map to Running/Succeeded/Failed/Pending;
// a removed Job keeps Succeeded, and turns Running into Failed.
func TestSyncBackup_JobStatesAndRemovedJob(t *testing.T) {
	p := &Provider{}
	ns := "test-ns"
	instName := "my-instance"
	schedName := "daily"

	now := metav1.NewTime(time.Now().Truncate(time.Second))
	later := metav1.NewTime(now.Add(10 * time.Minute))

	instance := testInstanceWithSchedule(instName, ns, "s3-storage", schedName, nil)
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	t.Run("Job active -> Running", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-running", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
		}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "job-running", Namespace: ns},
			Status: batchv1.JobStatus{
				Active:    1,
				StartTime: &now,
			},
		}
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup, job)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateRunning, status.State)
		assert.Equal(t, &now, status.StartedAt)
		require.NotNil(t, status.OperatorBackupRef)
		assert.Equal(t, "batch", status.OperatorBackupRef.Group)
		assert.Equal(t, "Job", status.OperatorBackupRef.Kind)
		assert.Equal(t, "job-running", status.OperatorBackupRef.Name)
	})

	t.Run("Job condition Complete -> Succeeded", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-succeeded", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
		}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "job-succeeded", Namespace: ns},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{
						Type:   batchv1.JobComplete,
						Status: corev1.ConditionTrue,
					},
				},
				StartTime:      &now,
				CompletionTime: &later,
			},
		}
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup, job)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateSucceeded, status.State)
		assert.Equal(t, &now, status.StartedAt)
		assert.Equal(t, &later, status.CompletedAt)
	})

	t.Run("Job condition Failed -> Failed with condition message", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-failed", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
		}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "job-failed", Namespace: ns},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{
						Type:    batchv1.JobFailed,
						Status:  corev1.ConditionTrue,
						Message: "Job reached maximum backoff limit",
					},
				},
				StartTime:      &now,
				CompletionTime: &later,
			},
		}
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup, job)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Equal(t, "Job reached maximum backoff limit", status.Message)
		assert.Equal(t, &now, status.StartedAt)
		assert.Equal(t, &later, status.CompletedAt)
	})

	t.Run("Job no conditions or active -> Pending", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-pending", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
		}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "job-pending", Namespace: ns},
			Status:     batchv1.JobStatus{},
		}
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup, job)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStatePending, status.State)
	})

	t.Run("Removed Job keeps Succeeded", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-gone-succ", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
			Status: backupv1alpha1.BackupStatus{
				State:       backupv1alpha1.BackupStateSucceeded,
				StartedAt:   &now,
				CompletedAt: &later,
				OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
					Group: "batch",
					Kind:  "Job",
					Name:  "job-gone-succ",
				},
			},
		}
		// No Job in client (history limit removed it)
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateSucceeded, status.State)
		assert.Equal(t, &now, status.StartedAt)
		assert.Equal(t, &later, status.CompletedAt)
	})

	t.Run("Removed Job keeps Failed", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-gone-fail", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
			Status: backupv1alpha1.BackupStatus{
				State:   backupv1alpha1.BackupStateFailed,
				Message: "earlier failure",
			},
		}
		// No Job in client
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Equal(t, "earlier failure", status.Message)
	})

	t.Run("Removed Job turns Running into Failed", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-gone-running", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStateRunning,
			},
		}
		// No Job in client
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Equal(t, "backup Job no longer exists", status.Message)
	})

	t.Run("Removed Job turns Pending into Failed", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "job-gone-pending", Namespace: ns},
			Spec:       backupv1alpha1.BackupSpec{ScheduleName: schedName},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStatePending,
			},
		}
		c, _ := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Equal(t, "backup Job no longer exists", status.Message)
	})
}

// Acceptance criteria 4:
// Count retention N=2 with 4 Succeeded Backups:
// the 2 oldest are patched to Retain and deleted, the 2 newest remain; Backups of other schedules are untouched.
func TestSyncBackup_CountRetentionPruning(t *testing.T) {
	p := &Provider{}
	ns := "test-ns"
	instName := "my-instance"
	schedName := "daily"
	otherSched := "weekly"

	count := int32(2)
	retention := &corev1alpha1.BackupScheduleRetention{
		Type:  corev1alpha1.BackupScheduleRetentionTypeCount,
		Count: &count,
	}

	instance := testInstanceWithSchedule(instName, ns, "s3-storage", schedName, retention)
	// Add other schedule on the same instance
	instance.Spec.Backup.Storages[0].Schedules = append(instance.Spec.Backup.Storages[0].Schedules,
		corev1alpha1.InstanceBackupSchedule{
			Name:    otherSched,
			Enabled: true,
			Cron:    "0 0 * * 0",
		},
	)

	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	t0 := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	t1 := metav1.NewTime(t0)
	t2 := metav1.NewTime(t0.Add(1 * time.Hour))
	t3 := metav1.NewTime(t0.Add(2 * time.Hour))
	t4 := metav1.NewTime(t0.Add(3 * time.Hour))
	tOther := metav1.NewTime(t0.Add(4 * time.Hour))

	makeBackup := func(name, sched string, compTime metav1.Time) *backupv1alpha1.Backup {
		return &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				Origin: backupv1alpha1.BackupOrigin{
					Type:        backupv1alpha1.BackupOriginTypeInstance,
					InstanceRef: &commonv1alpha1.ObjectRef{Name: instName},
				},
				StorageRef:   commonv1alpha1.ObjectRef{Name: "s3-storage"},
				ClassRef:     commonv1alpha1.ObjectRef{Name: "ch-backup-class"},
				ScheduleName: sched,
			},
			Status: backupv1alpha1.BackupStatus{
				State:       backupv1alpha1.BackupStateSucceeded,
				CompletedAt: &compTime,
			},
		}
	}

	b1 := makeBackup("b1-oldest", schedName, t1)
	b2 := makeBackup("b2-older", schedName, t2)
	b3 := makeBackup("b3-newer", schedName, t3)
	b4 := makeBackup("b4-newest", schedName, t4)
	bOther := makeBackup("b-other-sched", otherSched, tOther)

	// Backing Job for b4 (the newest backup currently triggering sync)
	job4 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b4.Name,
			Namespace: ns,
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			},
			CompletionTime: &t4,
		},
	}

	// Capture patches and deletes to verify deletionPolicy was patched to Retain prior to deletion
	var deletedBackups []string
	deletedPolicies := make(map[string]backupv1alpha1.BackupDeletionPolicy)

	interceptFuncs := interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if b, ok := obj.(*backupv1alpha1.Backup); ok {
				deletedPolicies[b.Name] = b.Spec.DeletionPolicy
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if b, ok := obj.(*backupv1alpha1.Backup); ok {
				deletedBackups = append(deletedBackups, b.Name)
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}

	c, fakeClient := newMirrorTestContext(t, instance, interceptFuncs, chi, b1, b2, b3, b4, bOther, job4)

	status, err := p.SyncBackup(c, b4)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStateSucceeded, status.State)

	// Verify the 2 oldest (b1 and b2) were patched to Retain and deleted
	assert.ElementsMatch(t, []string{"b1-oldest", "b2-older"}, deletedBackups)
	assert.Equal(t, backupv1alpha1.BackupDeletionPolicyRetain, deletedPolicies["b1-oldest"])
	assert.Equal(t, backupv1alpha1.BackupDeletionPolicyRetain, deletedPolicies["b2-older"])

	// b1 and b2 should be NotFound in the client
	checkB1 := &backupv1alpha1.Backup{}
	err1 := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b1-oldest"}, checkB1)
	assert.True(t, apierrors.IsNotFound(err1))

	checkB2 := &backupv1alpha1.Backup{}
	err2 := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b2-older"}, checkB2)
	assert.True(t, apierrors.IsNotFound(err2))

	// The 2 newest (b3 and b4) remain untouched
	checkB3 := &backupv1alpha1.Backup{}
	err3 := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b3-newer"}, checkB3)
	require.NoError(t, err3)

	checkB4 := &backupv1alpha1.Backup{}
	err4 := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b4-newest"}, checkB4)
	require.NoError(t, err4)

	// Backup of other schedule remains untouched
	checkBOther := &backupv1alpha1.Backup{}
	errOther := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b-other-sched"}, checkBOther)
	require.NoError(t, errOther)
}

// Requirement 4: Make the scheduled path never create a ClickHouseBackup.
func TestSyncBackup_ScheduledPathNeverCreatesClickHouseBackup(t *testing.T) {
	p := &Provider{}
	ns := "test-ns"
	instName := "my-instance"
	schedName := "daily"

	instance := testInstanceWithSchedule(instName, ns, "s3-storage", schedName, nil)
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}
	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scheduled-run",
			Namespace: ns,
		},
		Spec: backupv1alpha1.BackupSpec{
			ScheduleName: schedName,
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scheduled-run",
			Namespace: ns,
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}

	c, fakeClient := newMirrorTestContext(t, instance, interceptor.Funcs{}, chi, backup, job)

	status, err := p.SyncBackup(c, backup)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStateRunning, status.State)

	// Assert ClickHouseBackup was NOT created
	chb := &chiv1.ClickHouseBackup{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "scheduled-run"}, chb)
	assert.True(t, apierrors.IsNotFound(err), "ClickHouseBackup should not be created for scheduled backup")
}
