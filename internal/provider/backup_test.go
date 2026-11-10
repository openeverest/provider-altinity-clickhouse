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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newBackupTestContext(t *testing.T, instance *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, chiv1.AddToScheme(scheme))

	all := append([]client.Object{instance, testProvider(t)}, objs...)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&chiv1.ClickHouseBackup{}, &backupv1alpha1.Backup{}, &corev1alpha1.Instance{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				t := obj.GetCreationTimestamp()
				if t.IsZero() {
					obj.SetCreationTimestamp(metav1.Now())
				}
				return client.Create(ctx, obj, opts...)
			},
		}).
		Build()

	return controller.NewContext(context.Background(), fakeClient, instance, "altinity-clickhouse")
}

func testInstanceWithStorage(name, ns, storageName string) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1alpha1.InstanceSpec{
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled: true,
				Storages: []corev1alpha1.InstanceBackupStorage{
					{
						StorageRef: commonv1alpha1.ObjectRef{
							Name: storageName,
						},
					},
				},
			},
		},
	}
}

func TestSyncBackup_CreateCHBAndIdempotentSpec(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-ch"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: ns,
			Annotations: map[string]string{
				annotationRemoteBackupName: "custom-remote-name",
			},
		},
		Spec: backupv1alpha1.BackupSpec{
			StorageRef: commonv1alpha1.ObjectRef{
				Name: "s3-storage",
			},
			Parameters: &runtime.RawExtension{
				Raw: []byte(`{"replicaSelection":"AllReplicas"}`),
			},
		},
	}

	c := newBackupTestContext(t, instance, chi, backup)

	// First SyncBackup should create the ClickHouseBackup
	status, err := p.SyncBackup(c, backup)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStatePending, status.State)
	require.NotNil(t, status.OperatorBackupRef)
	assert.Equal(t, "clickhouse.altinity.com", status.OperatorBackupRef.Group)
	assert.Equal(t, "ClickHouseBackup", status.OperatorBackupRef.Kind)
	assert.Equal(t, backup.Name, status.OperatorBackupRef.Name)

	chb := &chiv1.ClickHouseBackup{}
	err = c.Client().Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.Name}, chb)
	require.NoError(t, err)
	assert.Equal(t, instName, chb.Spec.ClickHouseInstallation)
	assert.Equal(t, "custom-remote-name", chb.Spec.BackupName)
	assert.Equal(t, chiv1.ReplicaSelectionAllReplicas, chb.Spec.ReplicaSelection)
	require.Len(t, chb.OwnerReferences, 1)
	assert.Equal(t, backup.Name, chb.OwnerReferences[0].Name)
	assert.True(t, *chb.OwnerReferences[0].Controller)

	// Second SyncBackup: change parameters on backup, spec on CHB should not change
	backup.Spec.Parameters = &runtime.RawExtension{
		Raw: []byte(`{"replicaSelection":"FirstPerShard"}`),
	}
	backup.Annotations[annotationRemoteBackupName] = "different-remote-name"

	_, err = p.SyncBackup(c, backup)
	require.NoError(t, err)

	chbAfter := &chiv1.ClickHouseBackup{}
	err = c.Client().Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.Name}, chbAfter)
	require.NoError(t, err)
	assert.Equal(t, instName, chbAfter.Spec.ClickHouseInstallation)
	assert.Equal(t, "custom-remote-name", chbAfter.Spec.BackupName)
	assert.Equal(t, chiv1.ReplicaSelectionAllReplicas, chbAfter.Spec.ReplicaSelection)
}

func TestSyncBackup_PhaseMapping(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-ch"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute).Truncate(time.Second))
	completionTime := metav1.NewTime(time.Now().Truncate(time.Second))

	testCases := []struct {
		name          string
		phase         chiv1.OperationPhase
		conditions    []metav1.Condition
		clusters      []chiv1.OperationClusterStatus
		expectedState backupv1alpha1.BackupState
		expectMsg     bool
		expectedMsg   string
		expectTimes   bool
	}{
		{
			name:          "completed",
			phase:         chiv1.OperationPhaseCompleted,
			expectedState: backupv1alpha1.BackupStateSucceeded,
			expectTimes:   true,
		},
		{
			name:          "running",
			phase:         chiv1.OperationPhaseRunning,
			expectedState: backupv1alpha1.BackupStateRunning,
		},
		{
			name:          "pending",
			phase:         chiv1.OperationPhasePending,
			expectedState: backupv1alpha1.BackupStatePending,
		},
		{
			name:  "failed with false condition",
			phase: chiv1.OperationPhaseFailed,
			conditions: []metav1.Condition{
				{
					Type:    "Ready",
					Status:  metav1.ConditionFalse,
					Message: "snapshot upload failed",
				},
			},
			expectedState: backupv1alpha1.BackupStateFailed,
			expectMsg:     true,
			expectedMsg:   "snapshot upload failed",
		},
		{
			name:  "failed with cluster reason",
			phase: chiv1.OperationPhaseFailed,
			clusters: []chiv1.OperationClusterStatus{
				{
					Name:   "cluster-0",
					Reason: "JobFailed",
				},
			},
			expectedState: backupv1alpha1.BackupStateFailed,
			expectMsg:     true,
			expectedMsg:   "JobFailed",
		},
		{
			name:          "failed without condition or cluster reason",
			phase:         chiv1.OperationPhaseFailed,
			expectedState: backupv1alpha1.BackupStateFailed,
			expectMsg:     true,
			expectedMsg:   "ClickHouseBackup failed",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			backup := &backupv1alpha1.Backup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-backup-" + tc.name,
					Namespace: ns,
				},
				Spec: backupv1alpha1.BackupSpec{
					StorageRef: commonv1alpha1.ObjectRef{
						Name: "s3-storage",
					},
				},
			}

			chb := &chiv1.ClickHouseBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backup.Name,
					Namespace: ns,
				},
				Spec: chiv1.ClickHouseBackupSpec{
					ClickHouseInstallation: instName,
					BackupName:             backup.Name,
				},
				Status: chiv1.ClickHouseBackupStatus{
					Phase:          tc.phase,
					Conditions:     tc.conditions,
					Clusters:       tc.clusters,
					StartTime:      &startTime,
					CompletionTime: &completionTime,
				},
			}

			c := newBackupTestContext(t, instance, chi, backup, chb)

			status, err := p.SyncBackup(c, backup)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedState, status.State)
			if tc.expectMsg {
				assert.Equal(t, tc.expectedMsg, status.Message)
			}
			if tc.expectTimes {
				assert.Equal(t, &startTime, status.StartedAt)
				assert.Equal(t, &completionTime, status.CompletedAt)
			}
		})
	}
}

func TestSyncBackup_UnregisteredStorage(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-ch"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unregistered-backup",
			Namespace: ns,
		},
		Spec: backupv1alpha1.BackupSpec{
			StorageRef: commonv1alpha1.ObjectRef{
				Name: "other-storage",
			},
		},
	}

	c := newBackupTestContext(t, instance, chi, backup)

	status, err := p.SyncBackup(c, backup)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
	assert.Contains(t, status.Message, "other-storage")

	// Ensure no CHB was created
	chb := &chiv1.ClickHouseBackup{}
	err = c.Client().Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.Name}, chb)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestSyncBackup_MissingCHBTerminalAndNonTerminal(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-ch"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	started := metav1.NewTime(time.Now().Add(-15 * time.Minute))
	completed := metav1.NewTime(time.Now())

	t.Run("succeeded backup keeps succeeded when CHB missing", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "succ-backup",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				StorageRef: commonv1alpha1.ObjectRef{
					Name: "s3-storage",
				},
			},
			Status: backupv1alpha1.BackupStatus{
				State:       backupv1alpha1.BackupStateSucceeded,
				StartedAt:   &started,
				CompletedAt: &completed,
				OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
					Group: "clickhouse.altinity.com",
					Kind:  "ClickHouseBackup",
					Name:  "succ-backup",
				},
			},
		}

		c := newBackupTestContext(t, instance, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateSucceeded, status.State)
		assert.Equal(t, &started, status.StartedAt)
		assert.Equal(t, &completed, status.CompletedAt)
	})

	t.Run("running backup fails when CHB missing", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "running-backup",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				StorageRef: commonv1alpha1.ObjectRef{
					Name: "s3-storage",
				},
			},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStateRunning,
				OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
					Group: "clickhouse.altinity.com",
					Kind:  "ClickHouseBackup",
					Name:  "running-backup",
				},
			},
		}

		c := newBackupTestContext(t, instance, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Equal(t, "ClickHouseBackup is gone", status.Message)
	})
}

func TestSyncRestore_And_Cleanup(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-ch"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")

	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: ns,
		},
	}

	c := newBackupTestContext(t, instance)

	resStatus, err := p.SyncRestore(c, restore)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.RestoreStateFailed, resStatus.State)
	assert.Equal(t, "restore is not supported yet by this provider; use a ClickHouseRestore directly", resStatus.Message)

	done, err := p.CleanupRestore(c, restore)
	require.NoError(t, err)
	assert.True(t, done)

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cleanup-backup",
			Namespace: ns,
		},
	}
	chb := &chiv1.ClickHouseBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name,
			Namespace: ns,
		},
	}

	cBackup := newBackupTestContext(t, instance, backup, chb)

	// First cleanup: CHB exists, deletion is triggered, returns false
	cleanupDone, err := p.CleanupBackup(cBackup, backup)
	require.NoError(t, err)
	assert.False(t, cleanupDone)

	// Second cleanup: CHB is gone, returns true
	cleanupDone, err = p.CleanupBackup(cBackup, backup)
	require.NoError(t, err)
	assert.True(t, cleanupDone)
}

func TestSyncBackup_EdgeCases(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-ch"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")

	t.Run("CHI not found", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-backup",
				Namespace: ns,
			},
		}
		c := newBackupTestContext(t, instance, backup) // no CHI
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStatePending, status.State)
		assert.Equal(t, "Waiting for ClickHouseInstallation", status.Message)
	})

	t.Run("ScheduleName branch passthrough", func(t *testing.T) {
		chi := &chiv1.ClickHouseInstallation{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instName,
				Namespace: ns,
			},
		}
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scheduled-backup",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				ScheduleName: "hourly",
			},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStateRunning,
			},
		}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scheduled-backup",
				Namespace: ns,
			},
			Status: batchv1.JobStatus{
				Active: 1,
			},
		}
		c := newBackupTestContext(t, instance, chi, backup, job)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateRunning, status.State)
	})

	t.Run("Invalid JSON parameters", func(t *testing.T) {
		chi := &chiv1.ClickHouseInstallation{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instName,
				Namespace: ns,
			},
		}
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-params",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				StorageRef: commonv1alpha1.ObjectRef{
					Name: "s3-storage",
				},
				Parameters: &runtime.RawExtension{
					Raw: []byte("{not-json"),
				},
			},
		}
		c := newBackupTestContext(t, instance, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Contains(t, status.Message, "invalid backup parameters")

		chb := &chiv1.ClickHouseBackup{}
		err = c.Client().Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.Name}, chb)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("Invalid replicaSelection parameter", func(t *testing.T) {
		chi := &chiv1.ClickHouseInstallation{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instName,
				Namespace: ns,
			},
		}
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-replica-sel",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				StorageRef: commonv1alpha1.ObjectRef{
					Name: "s3-storage",
				},
				Parameters: &runtime.RawExtension{
					Raw: []byte(`{"replicaSelection":"InvalidMode"}`),
				},
			},
		}
		c := newBackupTestContext(t, instance, chi, backup)
		status, err := p.SyncBackup(c, backup)
		require.NoError(t, err)
		assert.Equal(t, backupv1alpha1.BackupStateFailed, status.State)
		assert.Contains(t, status.Message, `invalid replicaSelection "InvalidMode"`)
		assert.Contains(t, status.Message, string(chiv1.ReplicaSelectionFirstPerShard))
		assert.Contains(t, status.Message, string(chiv1.ReplicaSelectionAllReplicas))

		chb := &chiv1.ClickHouseBackup{}
		err = c.Client().Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.Name}, chb)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("BackupWatches", func(t *testing.T) {
		watches := p.BackupWatches()
		require.Len(t, watches, 1)
	})
}
