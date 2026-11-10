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
	"os/exec"
	"regexp"
	"strings"
	"testing"

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

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func newBackupDeleteTestContext(t *testing.T, instance *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
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
		WithStatusSubresource(&chiv1.ClickHouseBackup{}, &backupv1alpha1.Backup{}, &corev1alpha1.Instance{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				ts := obj.GetCreationTimestamp()
				if ts.IsZero() {
					obj.SetCreationTimestamp(metav1.Now())
				}
				return client.Create(ctx, obj, opts...)
			},
		}).
		Build()

	return controller.NewContext(context.Background(), fakeClient, instance, "altinity-clickhouse")
}

func TestCleanupBackup_DeleteSucceededTwoShards(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-inst"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")

	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
		Spec: chiv1.ChiSpec{
			Configuration: &chiv1.Configuration{
				Clusters: []*chiv1.Cluster{
					{
						Name: common.CHIClusterName,
						Layout: &chiv1.ChiClusterLayout{
							ShardsCount: 2,
						},
					},
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      autoBackupSecretName(instName),
			Namespace: ns,
		},
		Data: map[string][]byte{
			"API_USERNAME": []byte("test-user"),
			"API_PASSWORD": []byte("test-password"),
		},
	}

	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chi-test-inst-clickhouse-0-0-0",
			Namespace: ns,
			Labels: map[string]string{
				common.LabelCHIName: instName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "clickhouse-backup",
					Image: "altinity/clickhouse-backup:2.6.0",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: ns,
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{
					Name: instName,
				},
			},
			DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
		},
		Status: backupv1alpha1.BackupStatus{
			State: backupv1alpha1.BackupStateSucceeded,
		},
	}

	chb := &chiv1.ClickHouseBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name,
			Namespace: ns,
		},
	}

	c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup, chb)

	// First call: Job is created, returns false while it runs
	done, err := p.CleanupBackup(c, backup)
	require.NoError(t, err)
	assert.False(t, done, "CleanupBackup should return false while Job runs")

	// Verify Job was created
	job := &batchv1.Job{}
	jobName := backupDeletionJobName(backup.Name)
	err = c.Get(job, jobName)
	require.NoError(t, err)

	// Job is owned by Backup
	assert.True(t, metav1.IsControlledBy(job, backup), "Job should be owned by Backup")

	// Pod template has backup-chi label
	podLabels := job.Spec.Template.Labels
	require.NotNil(t, podLabels)
	assert.Equal(t, instName, podLabels[common.LabelBackupCHIName], "Pod template must have backup-chi label")

	// Pod spec configurations
	assert.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)
	require.NotNil(t, job.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(1800), *job.Spec.ActiveDeadlineSeconds)

	// Pod security context for Pod Security Standard restricted
	podSec := job.Spec.Template.Spec.SecurityContext
	require.NotNil(t, podSec, "Pod SecurityContext must be set")
	require.NotNil(t, podSec.RunAsNonRoot)
	assert.True(t, *podSec.RunAsNonRoot)
	require.NotNil(t, podSec.RunAsUser)
	assert.Equal(t, int64(65532), *podSec.RunAsUser)
	require.NotNil(t, podSec.RunAsGroup)
	assert.Equal(t, int64(65532), *podSec.RunAsGroup)
	require.NotNil(t, podSec.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, podSec.SeccompProfile.Type)

	// Container check
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "altinity/clickhouse-backup:2.6.0", container.Image)

	// Container security context for Pod Security Standard restricted
	cntSec := container.SecurityContext
	require.NotNil(t, cntSec, "Container SecurityContext must be set")
	require.NotNil(t, cntSec.AllowPrivilegeEscalation)
	assert.False(t, *cntSec.AllowPrivilegeEscalation)
	require.NotNil(t, cntSec.ReadOnlyRootFilesystem)
	assert.True(t, *cntSec.ReadOnlyRootFilesystem)
	require.NotNil(t, cntSec.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, cntSec.Capabilities.Drop)

	// Check environment variables
	var hostsEnv, userEnv, passEnv *corev1.EnvVar
	for i := range container.Env {
		env := &container.Env[i]
		switch env.Name {
		case "HOSTS":
			hostsEnv = env
		case "API_USERNAME":
			userEnv = env
		case "API_PASSWORD":
			passEnv = env
		}
	}

	require.NotNil(t, hostsEnv, "HOSTS env var must be set")
	assert.Contains(t, hostsEnv.Value, "chi-test-inst-clickhouse-0-0")
	assert.Contains(t, hostsEnv.Value, "chi-test-inst-clickhouse-1-0")

	// Credentials only through secretKeyRef, never in Value
	require.NotNil(t, userEnv, "API_USERNAME env var must be set")
	assert.Empty(t, userEnv.Value)
	require.NotNil(t, userEnv.ValueFrom)
	require.NotNil(t, userEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, autoBackupSecretName(instName), userEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "API_USERNAME", userEnv.ValueFrom.SecretKeyRef.Key)

	require.NotNil(t, passEnv, "API_PASSWORD env var must be set")
	assert.Empty(t, passEnv.Value)
	require.NotNil(t, passEnv.ValueFrom)
	require.NotNil(t, passEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, autoBackupSecretName(instName), passEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "API_PASSWORD", passEnv.ValueFrom.SecretKeyRef.Key)

	// While Job is still running, CleanupBackup continues to return false
	done, err = p.CleanupBackup(c, backup)
	require.NoError(t, err)
	assert.False(t, done)
}

func TestCleanupBackup_JobCompleteAndFailed(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-inst"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")

	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      autoBackupSecretName(instName),
			Namespace: ns,
		},
	}

	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chi-test-inst-clickhouse-0-0-0",
			Namespace: ns,
			Labels: map[string]string{
				common.LabelCHIName: instName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "clickhouse-backup",
					Image: "altinity/clickhouse-backup:2.6.0",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: ns,
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{
					Name: instName,
				},
			},
			DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
		},
		Status: backupv1alpha1.BackupStatus{
			State: backupv1alpha1.BackupStateSucceeded,
		},
	}

	t.Run("Job complete: ClickHouseBackup is deleted and eventually returns true", func(t *testing.T) {
		chb := &chiv1.ClickHouseBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      backup.Name,
				Namespace: ns,
			},
		}

		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup, chb)

		// First call creates Job
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.False(t, done)

		// Mark Job Complete
		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		require.NoError(t, err)
		job.Status.Conditions = []batchv1.JobCondition{
			{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			},
		}
		require.NoError(t, c.Client().Status().Update(c.Context(), job))

		// Next call: Job is complete, ClickHouseBackup deletion is triggered, returns false
		done, err = p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.False(t, done)

		// Next call: ClickHouseBackup is gone, returns true
		done, err = p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)
	})

	t.Run("Job failed: returns an error naming the Job", func(t *testing.T) {
		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup)

		// Create Job
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.False(t, done)

		// Mark Job Failed
		job := &batchv1.Job{}
		jobName := backupDeletionJobName(backup.Name)
		err = c.Get(job, jobName)
		require.NoError(t, err)
		job.Status.Conditions = []batchv1.JobCondition{
			{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
			},
		}
		require.NoError(t, c.Client().Status().Update(c.Context(), job))

		// CleanupBackup returns error naming the Job
		done, err = p.CleanupBackup(c, backup)
		assert.False(t, done)
		require.Error(t, err)
		assert.Contains(t, err.Error(), jobName, "error must name the failed Job")
	})
}

func TestCleanupBackup_NoJobWhenNotApplicable(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-inst"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")

	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      autoBackupSecretName(instName),
			Namespace: ns,
		},
	}

	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chi-test-inst-clickhouse-0-0-0",
			Namespace: ns,
			Labels: map[string]string{
				common.LabelCHIName: instName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "clickhouse-backup",
					Image: "altinity/clickhouse-backup:2.6.0",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	t.Run("Retain policy creates no Job", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-retain",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				Origin: backupv1alpha1.BackupOrigin{
					Type: backupv1alpha1.BackupOriginTypeInstance,
					InstanceRef: &commonv1alpha1.ObjectRef{
						Name: instName,
					},
				},
				DeletionPolicy: backupv1alpha1.BackupDeletionPolicyRetain,
			},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStateSucceeded,
			},
		}
		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err), "Job should not be created for Retain")
	})

	t.Run("External origin creates no Job", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-external",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				Origin: backupv1alpha1.BackupOrigin{
					Type: backupv1alpha1.BackupOriginTypeExternal,
				},
				DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
			},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStateSucceeded,
			},
		}
		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err), "Job should not be created for External origin")
	})

	t.Run("Pending backup creates no Job", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pending",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				Origin: backupv1alpha1.BackupOrigin{
					Type: backupv1alpha1.BackupOriginTypeInstance,
					InstanceRef: &commonv1alpha1.ObjectRef{
						Name: instName,
					},
				},
				DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
			},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStatePending,
			},
		}
		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err), "Job should not be created for Pending backup")
	})

	t.Run("Failed backup without OperatorBackupRef creates no Job", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-failed-no-ref",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				Origin: backupv1alpha1.BackupOrigin{
					Type: backupv1alpha1.BackupOriginTypeInstance,
					InstanceRef: &commonv1alpha1.ObjectRef{
						Name: instName,
					},
				},
				DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
			},
			Status: backupv1alpha1.BackupStatus{
				State:             backupv1alpha1.BackupStateFailed,
				OperatorBackupRef: nil,
			},
		}
		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err), "Job should not be created for Failed backup without OperatorBackupRef")
	})

	t.Run("Failed backup with OperatorBackupRef creates Job", func(t *testing.T) {
		backup := &backupv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-failed-with-ref",
				Namespace: ns,
			},
			Spec: backupv1alpha1.BackupSpec{
				Origin: backupv1alpha1.BackupOrigin{
					Type: backupv1alpha1.BackupOriginTypeInstance,
					InstanceRef: &commonv1alpha1.ObjectRef{
						Name: instName,
					},
				},
				DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
			},
			Status: backupv1alpha1.BackupStatus{
				State: backupv1alpha1.BackupStateFailed,
				OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
					Group: "clickhouse.altinity.com",
					Kind:  "ClickHouseBackup",
					Name:  "test-failed-with-ref",
				},
			},
		}
		c := newBackupDeleteTestContext(t, instance, chi, secret, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.False(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		require.NoError(t, err, "Job should be created for Failed backup with OperatorBackupRef")
	})
}

func TestCleanupBackup_MissingPrerequisites(t *testing.T) {
	p := &Provider{}
	ns := "default"
	instName := "test-inst"
	instance := testInstanceWithStorage(instName, ns, "s3-storage")

	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instName,
			Namespace: ns,
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      autoBackupSecretName(instName),
			Namespace: ns,
		},
	}

	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chi-test-inst-clickhouse-0-0-0",
			Namespace: ns,
			Labels: map[string]string{
				common.LabelCHIName: instName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "clickhouse-backup",
					Image: "altinity/clickhouse-backup:2.6.0",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: ns,
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{
					Name: instName,
				},
			},
			DeletionPolicy: backupv1alpha1.BackupDeletionPolicyDelete,
		},
		Status: backupv1alpha1.BackupStatus{
			State: backupv1alpha1.BackupStateSucceeded,
		},
	}

	t.Run("CHI missing: no Job, returns true", func(t *testing.T) {
		// No CHI provided
		c := newBackupDeleteTestContext(t, instance, secret, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("Pod missing: no Job, returns true", func(t *testing.T) {
		// No Pod provided
		c := newBackupDeleteTestContext(t, instance, chi, secret, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("Auto-backup secret missing: no Job, returns true", func(t *testing.T) {
		// No Secret provided
		c := newBackupDeleteTestContext(t, instance, chi, runningPod, backup)
		done, err := p.CleanupBackup(c, backup)
		require.NoError(t, err)
		assert.True(t, done)

		job := &batchv1.Job{}
		err = c.Get(job, backupDeletionJobName(backup.Name))
		assert.True(t, apierrors.IsNotFound(err))
	})
}

func TestBackupDeletionJobName(t *testing.T) {
	t.Run("short name appends -delete", func(t *testing.T) {
		name := backupDeletionJobName("daily-backup")
		assert.Equal(t, "daily-backup-delete", name)
		assert.LessOrEqual(t, len(name), 63)
	})

	t.Run("name longer than 63 chars yields valid stable job name", func(t *testing.T) {
		longName := "backup-with-a-very-long-name-that-definitely-exceeds-sixty-three-characters-limit"
		name1 := backupDeletionJobName(longName)
		name2 := backupDeletionJobName(longName)

		assert.Equal(t, name1, name2, "job name must be stable")
		assert.LessOrEqual(t, len(name1), 63, "job name must be <= 63 characters")

		// Verify RFC 1123 label format: lowercase alphanumeric, '-', start/end with alphanumeric
		dnsRegex := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
		assert.True(t, dnsRegex.MatchString(name1), "job name %q must be valid RFC 1123 label", name1)

		// Different long name yields different hash
		diffLongName := "backup-with-a-very-long-name-that-definitely-exceeds-sixty-three-characters-other"
		nameDiff := backupDeletionJobName(diffLongName)
		assert.NotEqual(t, name1, nameDiff)
		assert.LessOrEqual(t, len(nameDiff), 63)
	})
}

func TestBackupDeletionScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(backupDeletionScript)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

func TestBuildBackupDeletionJob_PodSecurityStandardRestricted(t *testing.T) {
	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily-backup",
			Namespace: "db-ns",
		},
	}
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-chi",
			Namespace: "db-ns",
		},
	}
	job, err := buildBackupDeletionJob(backup, "my-chi", chi, "altinity/clickhouse-backup:2.6.0")
	require.NoError(t, err)
	require.NotNil(t, job)

	// Pod securityContext assertions
	podSec := job.Spec.Template.Spec.SecurityContext
	require.NotNil(t, podSec, "Pod SecurityContext must be configured")
	require.NotNil(t, podSec.RunAsNonRoot, "runAsNonRoot must be set")
	assert.True(t, *podSec.RunAsNonRoot, "runAsNonRoot must be true")
	require.NotNil(t, podSec.RunAsUser, "runAsUser must be set")
	assert.Equal(t, int64(65532), *podSec.RunAsUser, "runAsUser must be 65532")
	require.NotNil(t, podSec.RunAsGroup, "runAsGroup must be set")
	assert.Equal(t, int64(65532), *podSec.RunAsGroup, "runAsGroup must be 65532")
	require.NotNil(t, podSec.SeccompProfile, "seccompProfile must be set")
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, podSec.SeccompProfile.Type, "seccompProfile.type must be RuntimeDefault")

	// Container securityContext assertions
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	cntSec := container.SecurityContext
	require.NotNil(t, cntSec, "Container SecurityContext must be configured")
	require.NotNil(t, cntSec.AllowPrivilegeEscalation, "allowPrivilegeEscalation must be set")
	assert.False(t, *cntSec.AllowPrivilegeEscalation, "allowPrivilegeEscalation must be false")
	require.NotNil(t, cntSec.ReadOnlyRootFilesystem, "readOnlyRootFilesystem must be set")
	assert.True(t, *cntSec.ReadOnlyRootFilesystem, "readOnlyRootFilesystem must be true")
	require.NotNil(t, cntSec.Capabilities, "capabilities must be set")
	assert.Equal(t, []corev1.Capability{"ALL"}, cntSec.Capabilities.Drop, "capabilities.drop must contain ALL")
}

