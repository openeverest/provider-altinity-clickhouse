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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/model/chbackup"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

var _ controller.BackupMirror = (*Provider)(nil)

// OperatorBackupType implements controller.BackupMirror.
func (p *Provider) OperatorBackupType() client.Object {
	return &batchv1.Job{}
}

// Mirror implements controller.BackupMirror. It mirrors backup Jobs started by
// ClickHouseBackupSchedule CronJobs into OpenEverest Backup CRs.
func (p *Provider) Mirror(ctx context.Context, c client.Client, obj client.Object) (*backupv1alpha1.Backup, error) {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return nil, nil
	}

	if job.Labels == nil || job.Labels[chbackup.LabelBackupCRKind] != "ClickHouseBackupSchedule" {
		return nil, nil
	}

	chbsName := job.Labels[chbackup.LabelBackupCRName]
	if chbsName == "" {
		return nil, nil
	}

	chbs := &chiv1.ClickHouseBackupSchedule{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: chbsName}, chbs); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ClickHouseBackupSchedule %q: %w", chbsName, err)
	}
	if chbs.Namespace == "" {
		chbs.Namespace = job.Namespace
	}

	owner := metav1.GetControllerOf(chbs)
	if owner == nil || owner.Kind != "Instance" {
		return nil, nil
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil || gv.Group != corev1alpha1.GroupVersion.Group {
		return nil, nil
	}

	instance := &corev1alpha1.Instance{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: owner.Name}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get parent Instance %q: %w", owner.Name, err)
	}

	if instance.Spec.ProviderRef.Name != common.ProviderName {
		return nil, nil
	}
	if instance.Spec.Backup == nil || instance.Spec.Backup.ClassRef.Name == "" {
		return nil, nil
	}
	if len(instance.Spec.Backup.Storages) == 0 {
		return nil, nil
	}

	var matchedSchedule *corev1alpha1.InstanceBackupSchedule
	for _, storage := range instance.Spec.Backup.Storages {
		for i := range storage.Schedules {
			s := &storage.Schedules[i]
			if instance.Name+"-"+s.Name == chbs.Name {
				matchedSchedule = s
				break
			}
		}
		if matchedSchedule != nil {
			break
		}
	}
	if matchedSchedule == nil {
		return nil, nil
	}

	cronJobOwner := metav1.GetControllerOf(job)
	if cronJobOwner == nil || cronJobOwner.Kind != "CronJob" {
		for _, ref := range job.OwnerReferences {
			if ref.Kind == "CronJob" {
				cronJobOwner = &ref
				break
			}
		}
	}
	if cronJobOwner == nil || cronJobOwner.Kind != "CronJob" {
		return nil, nil
	}

	prefix := cronJobOwner.Name + "-"
	slot, found := strings.CutPrefix(job.Name, prefix)
	l := log.FromContext(ctx)
	if !found || slot == "" {
		l.Info("Skipping job: name does not match controller CronJob prefix", "job", job.Name, "cronJob", cronJobOwner.Name)
		return nil, nil
	}

	for _, r := range slot {
		if r < '0' || r > '9' {
			l.Info("Skipping job: slot is not numeric", "job", job.Name, "slot", slot)
			return nil, nil
		}
	}

	slotInt, err := strconv.ParseInt(slot, 10, 64)
	if err != nil {
		l.Info("Skipping job: slot is not numeric", "job", job.Name, "slot", slot)
		return nil, nil
	}

	if chbs.Spec.ClickHouseInstallation == "" {
		chbs.Spec.ClickHouseInstallation = instance.Name
	}

	dueTime := time.Unix(slotInt*60, 0).UTC()
	remoteName := fmt.Sprintf("%s-%s", chbackup.BackupNamePrefix(chbs), dueTime.Format("20060102-150405"))

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
			Annotations: map[string]string{
				annotationRemoteBackupName: remoteName,
			},
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: instance.Name},
			},
			ClassRef:     commonv1alpha1.ObjectRef{Name: instance.Spec.Backup.ClassRef.Name},
			StorageRef:   commonv1alpha1.ObjectRef{Name: instance.Spec.Backup.Storages[0].StorageRef.Name},
			ScheduleName: matchedSchedule.Name,
			Parameters:   matchedSchedule.Parameters,
		},
	}

	return backup, nil
}

// syncScheduledBackup updates a scheduled backup's status from its backing batchv1.Job
// and prunes expired backups when retention is configured.
func syncScheduledBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	job := &batchv1.Job{}
	if err := c.Get(job, backup.Name); err != nil {
		if apierrors.IsNotFound(err) {
			if backup.Status.State == backupv1alpha1.BackupStateSucceeded || backup.Status.State == backupv1alpha1.BackupStateFailed {
				return controller.BackupExecutionStatus{
					State:             backup.Status.State,
					Message:           backup.Status.Message,
					StartedAt:         backup.Status.StartedAt,
					CompletedAt:       backup.Status.CompletedAt,
					OperatorBackupRef: backup.Status.OperatorBackupRef,
				}, nil
			}
			return controller.BackupExecutionStatus{
				State:             backupv1alpha1.BackupStateFailed,
				Message:           "backup Job no longer exists",
				OperatorBackupRef: backup.Status.OperatorBackupRef,
			}, nil
		}
		return controller.BackupExecutionStatus{}, fmt.Errorf("get backup Job %q: %w", backup.Name, err)
	}

	exec := controller.BackupExecutionStatus{
		OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
			Group: "batch",
			Kind:  "Job",
			Name:  job.Name,
		},
	}

	var completeCond *batchv1.JobCondition
	var failedCond *batchv1.JobCondition
	for i := range job.Status.Conditions {
		cond := &job.Status.Conditions[i]
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			completeCond = cond
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			failedCond = cond
		}
	}

	if job.Status.Active > 0 {
		exec.State = backupv1alpha1.BackupStateRunning
		exec.StartedAt = job.Status.StartTime
	} else if completeCond != nil {
		exec.State = backupv1alpha1.BackupStateSucceeded
		exec.StartedAt = job.Status.StartTime
		exec.CompletedAt = job.Status.CompletionTime
	} else if failedCond != nil {
		exec.State = backupv1alpha1.BackupStateFailed
		exec.StartedAt = job.Status.StartTime
		exec.CompletedAt = job.Status.CompletionTime
		exec.Message = failedCond.Message
		if exec.Message == "" {
			if failedCond.Reason != "" {
				exec.Message = failedCond.Reason
			} else {
				exec.Message = "backup Job failed"
			}
		}
	} else {
		exec.State = backupv1alpha1.BackupStatePending
	}

	if exec.State == backupv1alpha1.BackupStateSucceeded {
		if err := pruneScheduledBackups(c, backup, exec); err != nil {
			return exec, err
		}
	}

	return exec, nil
}

func pruneScheduledBackups(c *controller.Context, backup *backupv1alpha1.Backup, exec controller.BackupExecutionStatus) error {
	instance := c.Instance()
	if instance == nil || instance.Spec.Backup == nil {
		return nil
	}

	var matchedSchedule *corev1alpha1.InstanceBackupSchedule
	for _, storage := range instance.Spec.Backup.Storages {
		for i := range storage.Schedules {
			s := &storage.Schedules[i]
			if s.Name == backup.Spec.ScheduleName {
				matchedSchedule = s
				break
			}
		}
		if matchedSchedule != nil {
			break
		}
	}
	if matchedSchedule == nil || matchedSchedule.Retention == nil {
		return nil
	}
	if matchedSchedule.Retention.Type != corev1alpha1.BackupScheduleRetentionTypeCount || matchedSchedule.Retention.Count == nil {
		return nil
	}
	n := int(*matchedSchedule.Retention.Count)
	if n <= 0 {
		return nil
	}

	list := &backupv1alpha1.BackupList{}
	if err := c.Client().List(c.Context(), list, client.InNamespace(c.Namespace())); err != nil {
		return fmt.Errorf("list backups for retention pruning: %w", err)
	}

	// shortcut: counts only Succeeded Backups while the operator's keepLast counts only backups that hold data; the two can differ by the runs without data.
	var succeeded []backupv1alpha1.Backup
	foundCurrent := false
	for _, b := range list.Items {
		if !b.DeletionTimestamp.IsZero() {
			continue
		}
		if b.Spec.Origin.InstanceRef != nil && b.Spec.Origin.InstanceRef.Name != c.Name() {
			continue
		}
		if b.Spec.ScheduleName != backup.Spec.ScheduleName {
			continue
		}
		state := b.Status.State
		completedAt := b.Status.CompletedAt
		if b.Name == backup.Name {
			foundCurrent = true
			state = exec.State
			if exec.CompletedAt != nil {
				completedAt = exec.CompletedAt
			}
		}
		if state != backupv1alpha1.BackupStateSucceeded {
			continue
		}
		bCopy := b
		bCopy.Status.CompletedAt = completedAt
		succeeded = append(succeeded, bCopy)
	}

	if !foundCurrent && exec.State == backupv1alpha1.BackupStateSucceeded &&
		(backup.Spec.Origin.InstanceRef == nil || backup.Spec.Origin.InstanceRef.Name == c.Name()) {
		bCopy := *backup.DeepCopy()
		bCopy.Status.State = exec.State
		bCopy.Status.CompletedAt = exec.CompletedAt
		succeeded = append(succeeded, bCopy)
	}

	if len(succeeded) <= n {
		return nil
	}

	sort.SliceStable(succeeded, func(i, j int) bool {
		t1 := succeeded[i].Status.CompletedAt
		t2 := succeeded[j].Status.CompletedAt
		if t1 == nil && t2 == nil {
			return succeeded[i].CreationTimestamp.After(succeeded[j].CreationTimestamp.Time)
		}
		if t1 == nil {
			return false
		}
		if t2 == nil {
			return true
		}
		if t1.Equal(t2) {
			return succeeded[i].Name > succeeded[j].Name
		}
		return t1.After(t2.Time)
	})

	for i := n; i < len(succeeded); i++ {
		target := succeeded[i]
		patch := client.MergeFrom(target.DeepCopy())
		target.Spec.DeletionPolicy = backupv1alpha1.BackupDeletionPolicyRetain
		if err := c.Client().Patch(c.Context(), &target, patch); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("patch backup %q deletion policy: %w", target.Name, err)
			}
			continue
		}
		if err := c.Client().Delete(c.Context(), &target); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete backup %q: %w", target.Name, err)
			}
		}
	}
	return nil
}
