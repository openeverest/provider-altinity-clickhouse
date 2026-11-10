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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// backupDeletionScript asks each shard's sidecar to delete the remote backup.
// shortcut: the sidecar API is asynchronous, so a 2xx reply only means the deletion was accepted
// and a failure inside clickhouse-backup is not reported back; poll /backup/actions for the
// operation result if silent leftovers in the storage turn out to matter.
const backupDeletionScript = `set -euo pipefail

: "${BACKUP_NAME:?BACKUP_NAME must be set}"
: "${HOSTS:?HOSTS must be set}"
: "${API_USERNAME:?API_USERNAME must be set}"
: "${API_PASSWORD:?API_PASSWORD must be set}"

PORT="${API_PORT:-7171}"

for host in ${HOSTS}; do
  while :; do
    curl_exit=0
    out=$(curl -sS -w "\n%{http_code}" -u "${API_USERNAME}:${API_PASSWORD}" --connect-timeout 10 -X POST "http://${host}:${PORT}/backup/delete/remote/${BACKUP_NAME}" 2>&1) || curl_exit=$?
    if [ "${curl_exit}" -ne 0 ]; then
      echo "Failed to connect to ${host}:${PORT} (curl exit ${curl_exit}): ${out}" >&2
      sleep 5
      continue
    fi
    http_code=$(echo "${out}" | tail -n 1)
    http_body=$(echo "${out}" | sed '$ d')
    if [[ "${http_code}" =~ ^2 ]]; then
      break
    fi
    if [ "${http_code}" = "409" ] || [ "${http_code}" = "423" ] || [[ "${http_body}" == *"another operation is currently running"* ]]; then
      sleep 5
      continue
    fi
    if [[ "${http_body}" == *"not found on remote storage"* ]]; then
      break
    fi
    echo "Delete remote backup ${BACKUP_NAME} on ${host} failed (HTTP ${http_code}): ${http_body}" >&2
    exit 1
  done
  echo "${host}: deletion of remote backup ${BACKUP_NAME} acknowledged"
done
`

// shouldDeleteBackupData reports whether the Backup CR meets all criteria for deleting
// its remote storage data: deletionPolicy is Delete (or empty), origin is Instance, and
// the backup may hold data (Succeeded, or Failed/Error with OperatorBackupRef set).
func shouldDeleteBackupData(b *backupv1alpha1.Backup) bool {
	if b.Spec.DeletionPolicy != "" && b.Spec.DeletionPolicy != backupv1alpha1.BackupDeletionPolicyDelete {
		return false
	}
	if b.Spec.Origin.Type != backupv1alpha1.BackupOriginTypeInstance {
		return false
	}
	return backupMayHoldData(b)
}

func backupMayHoldData(b *backupv1alpha1.Backup) bool {
	switch b.Status.State {
	case backupv1alpha1.BackupStateSucceeded:
		return true
	case backupv1alpha1.BackupStateFailed, backupv1alpha1.BackupStateError:
		return b.Status.OperatorBackupRef != nil
	case backupv1alpha1.BackupStateDeleting:
		return b.Status.CompletedAt != nil || b.Status.OperatorBackupRef != nil
	default:
		return false
	}
}

// backupInstanceName resolves the Instance name for a backup.
func backupInstanceName(c *controller.Context, backup *backupv1alpha1.Backup) string {
	if backup.Spec.Origin.InstanceRef != nil && backup.Spec.Origin.InstanceRef.Name != "" {
		return backup.Spec.Origin.InstanceRef.Name
	}
	if c.Instance() != nil && c.Instance().Name != "" {
		return c.Instance().Name
	}
	return c.Name()
}

// backupDeletionJobName formats a Job name <backup.Name>-delete bounded to 63 characters,
// appending a stable hash suffix when truncated.
func backupDeletionJobName(backupName string) string {
	const suffix = "-delete"
	fullName := backupName + suffix
	if len(fullName) <= 63 {
		return fullName
	}
	const hashLength = 8
	sum := sha256.Sum256([]byte(backupName))
	hash := hex.EncodeToString(sum[:])[:hashLength]
	maxPrefix := 63 - hashLength - 1
	prefix := strings.TrimRight(fullName[:maxPrefix], "-.")
	return prefix + "-" + hash
}

// findClickHouseBackupImage returns the image of the clickhouse-backup container
// from any running pod with label clickhouse.altinity.com/chi=<instance>.
func findClickHouseBackupImage(ctx context.Context, cl client.Client, namespace, instance string) (string, error) {
	podList := &corev1.PodList{}
	if err := cl.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{common.LabelCHIName: instance},
	); err != nil {
		return "", fmt.Errorf("list ClickHouse pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "clickhouse-backup" && container.Image != "" {
				return container.Image, nil
			}
		}
	}
	return "", nil
}

// backupDeletionNonRootUID is the distroless nonroot user/group ID (65532) used
// so the backup deletion Job satisfies the Kubernetes Pod Security Standard "restricted".
const backupDeletionNonRootUID = int64(65532)

// buildBackupDeletionJob constructs the one-shot batchv1.Job for remote backup data deletion.
func buildBackupDeletionJob(
	backup *backupv1alpha1.Backup,
	instanceName string,
	chi *chiv1.ClickHouseInstallation,
	image string,
) (*batchv1.Job, error) {
	shards := liveShardsCount(chi)
	if shards < 1 {
		shards = 1
	}
	hosts := make([]string, shards)
	for s := 0; s < shards; s++ {
		hosts[s] = fmt.Sprintf("chi-%s-%s-%d-0", instanceName, common.CHIClusterName, s)
	}

	secretName := autoBackupSecretName(instanceName)
	env := []corev1.EnvVar{
		{Name: "BACKUP_NAME", Value: remoteBackupName(backup)},
		{Name: "HOSTS", Value: strings.Join(hosts, " ")},
		{Name: "API_PORT", Value: strconv.Itoa(common.BackupAPIPort)},
		{
			Name: "API_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "API_USERNAME",
				},
			},
		},
		{
			Name: "API_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "API_PASSWORD",
				},
			},
		},
	}

	backoffLimit := int32(0)
	activeDeadlineSeconds := int64(1800)
	automount := false
	jobName := backupDeletionJobName(backup.Name)
	labels := map[string]string{
		common.LabelBackupCHIName: instanceName,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: backup.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(backupDeletionNonRootUID),
						RunAsGroup:   ptr.To(backupDeletionNonRootUID),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "backup-delete",
							Image:   image,
							Command: []string{"bash", "-c", backupDeletionScript},
							Env:     env,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								ReadOnlyRootFilesystem:   ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
						},
					},
				},
			},
		},
	}, nil
}

// deleteBackupData orchestrates remote backup data deletion via a one-shot Job.
// Returns done = true when the Job completed.
// Returns missingInstance = true if the CHI, auto-backup Secret, or running ClickHouse pod is missing.
func (p *Provider) deleteBackupData(c *controller.Context, backup *backupv1alpha1.Backup) (done bool, missingInstance bool, err error) {
	l := log.FromContext(c.Context())
	jobName := backupDeletionJobName(backup.Name)
	job := &batchv1.Job{}
	err = c.Get(job, jobName)
	if err == nil {
		if !metav1.IsControlledBy(job, backup) {
			return false, false, fmt.Errorf("backup data deletion Job %q is not owned by Backup %q", jobName, backup.Name)
		}
		done, dErr := deletionJobDone(job)
		return done, false, dErr
	}
	if !apierrors.IsNotFound(err) {
		return false, false, fmt.Errorf("get backup data deletion Job %q: %w", jobName, err)
	}

	instanceName := backupInstanceName(c, backup)
	chi := &chiv1.ClickHouseInstallation{}
	if err := c.Get(chi, instanceName); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("ClickHouseInstallation is missing; skipping backup data deletion",
				"instance", instanceName, "backup", backup.Name)
			return true, true, nil
		}
		return false, false, fmt.Errorf("get ClickHouseInstallation %q: %w", instanceName, err)
	}

	secretName := autoBackupSecretName(instanceName)
	secret := &corev1.Secret{}
	if err := c.Get(secret, secretName); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("Auto backup secret is missing; skipping backup data deletion",
				"secret", secretName, "backup", backup.Name)
			return true, true, nil
		}
		return false, false, fmt.Errorf("get auto backup Secret %q: %w", secretName, err)
	}

	image, err := findClickHouseBackupImage(c.Context(), c.Client(), backup.Namespace, instanceName)
	if err != nil {
		return false, false, fmt.Errorf("find running ClickHouse backup image: %w", err)
	}
	if image == "" {
		l.Info("Running ClickHouse pod with clickhouse-backup container is missing; skipping backup data deletion",
			"instance", instanceName, "backup", backup.Name)
		return true, true, nil
	}

	job, err = buildBackupDeletionJob(backup, instanceName, chi, image)
	if err != nil {
		return false, false, err
	}
	if err := controllerutil.SetControllerReference(backup, job, c.Client().Scheme()); err != nil {
		return false, false, fmt.Errorf("set owner for backup data deletion Job: %w", err)
	}
	if err := c.Client().Create(c.Context(), job); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, false, fmt.Errorf("create backup data deletion Job %q: %w", jobName, err)
	}
	return false, false, nil
}

func deletionJobDone(job *batchv1.Job) (bool, error) {
	if jobFailed(job) {
		return false, fmt.Errorf("backup data deletion Job %q failed; inspect its logs, then delete the Job to retry", job.Name)
	}
	return jobComplete(job), nil
}

func jobComplete(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
