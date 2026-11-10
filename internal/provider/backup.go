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
	"encoding/json"
	"fmt"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Compile-time interface checks.
var _ controller.BackupProvider = (*Provider)(nil)
var _ controller.BackupWatcher = (*Provider)(nil)

const annotationRemoteBackupName = "provider.openeverest.io/remote-backup-name"

// remoteBackupName is the backup's name in the storage: the annotation, set on
// mirrored scheduled backups, or else the Backup CR's name.
func remoteBackupName(b *backupv1alpha1.Backup) string {
	if b.Annotations != nil {
		if name := b.Annotations[annotationRemoteBackupName]; name != "" {
			return name
		}
	}
	return b.Name
}

type backupParameters struct {
	ReplicaSelection string `json:"replicaSelection"`
}

func decodeBackupParameters(raw *runtime.RawExtension) (backupParameters, error) {
	if raw == nil || (len(raw.Raw) == 0 && raw.Object == nil) {
		return backupParameters{}, nil
	}
	var params backupParameters
	if len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &params); err != nil {
			return backupParameters{}, fmt.Errorf("invalid backup parameters: %w", err)
		}
	} else if raw.Object != nil {
		data, err := json.Marshal(raw.Object)
		if err != nil {
			return backupParameters{}, fmt.Errorf("marshal backup parameters: %w", err)
		}
		if err := json.Unmarshal(data, &params); err != nil {
			return backupParameters{}, fmt.Errorf("invalid backup parameters: %w", err)
		}
	}
	return params, nil
}

func instanceHasBackupStorage(instance *corev1alpha1.Instance, storageName string) bool {
	if instance == nil {
		return false
	}
	backup := instance.Spec.Backup
	if backup == nil || !backup.Enabled {
		return false
	}
	for _, storage := range backup.Storages {
		if storage.StorageRef.Name == storageName {
			return true
		}
	}
	return false
}

func backupFailureMessage(status *chiv1.ClickHouseBackupStatus) string {
	for _, c := range status.Conditions {
		if (c.Status == metav1.ConditionFalse || c.Type == "Failed" || c.Reason == "Failed") && c.Message != "" {
			return c.Message
		}
	}
	for _, cl := range status.Clusters {
		if cl.Reason != "" {
			return cl.Reason
		}
		if cl.Message != "" {
			return cl.Message
		}
	}
	for _, c := range status.Conditions {
		if (c.Status == metav1.ConditionFalse || c.Type == "Failed" || c.Reason == "Failed") && c.Reason != "" {
			return c.Reason
		}
	}
	return "ClickHouseBackup failed"
}

// SyncBackup creates or updates the operator's ClickHouseBackup resource, sets a controller
// reference from the Backup CR to enable owner-based watches, and maps operator status to
// OpenEverest states.
func (p *Provider) SyncBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	l := log.FromContext(c.Context())
	l.Info("Syncing backup", "name", backup.Name)

	if backup.Namespace == "" {
		backup.Namespace = c.Namespace()
	}

	chi := &chiv1.ClickHouseInstallation{}
	if err := c.Get(chi, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStatePending,
				Message: "Waiting for ClickHouseInstallation",
			}, nil
		}
		return controller.BackupExecutionStatus{}, fmt.Errorf("get ClickHouseInstallation: %w", err)
	}

	if backup.Spec.ScheduleName != "" {
		return syncScheduledBackup(c, backup)
	}

	exists, err := c.Exists(&chiv1.ClickHouseBackup{}, backup.Name)
	if err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("get ClickHouseBackup: %w", err)
	}

	if !exists {
		if backup.Status.OperatorBackupRef != nil {
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
				Message:           "ClickHouseBackup is gone",
				OperatorBackupRef: backup.Status.OperatorBackupRef,
			}, nil
		}
		if !instanceHasBackupStorage(c.Instance(), backup.Spec.StorageRef.Name) {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStateFailed,
				Message: fmt.Sprintf("storage %q is not registered and enabled on Instance %q", backup.Spec.StorageRef.Name, c.Name()),
			}, nil
		}
	}

	params, err := decodeBackupParameters(backup.Spec.Parameters)
	if err != nil {
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateFailed,
			Message: err.Error(),
		}, nil
	}
	if params.ReplicaSelection != "" &&
		params.ReplicaSelection != string(chiv1.ReplicaSelectionFirstPerShard) &&
		params.ReplicaSelection != string(chiv1.ReplicaSelectionAllReplicas) {
		return controller.BackupExecutionStatus{
			State: backupv1alpha1.BackupStateFailed,
			Message: fmt.Sprintf("invalid replicaSelection %q: allowed values are %q and %q",
				params.ReplicaSelection, chiv1.ReplicaSelectionFirstPerShard, chiv1.ReplicaSelectionAllReplicas),
		}, nil
	}

	chb := &chiv1.ClickHouseBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name,
			Namespace: backup.Namespace,
		},
	}

	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), chb, func() error {
		if chb.CreationTimestamp.IsZero() {
			spec := chiv1.ClickHouseBackupSpec{
				ClickHouseInstallation: c.Name(),
				BackupName:             remoteBackupName(backup),
			}
			if params.ReplicaSelection != "" {
				spec.ReplicaSelection = chiv1.ReplicaSelection(params.ReplicaSelection)
			}
			chb.Spec = spec
		}
		return controllerutil.SetControllerReference(backup, chb, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create or update ClickHouseBackup: %w", err)
	}

	exec := controller.BackupExecutionStatus{
		OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
			Group: "clickhouse.altinity.com",
			Kind:  "ClickHouseBackup",
			Name:  chb.Name,
		},
		State: backupv1alpha1.BackupStatePending,
	}

	switch chb.Status.Phase {
	case chiv1.OperationPhaseCompleted:
		exec.State = backupv1alpha1.BackupStateSucceeded
		exec.StartedAt = chb.Status.StartTime
		exec.CompletedAt = chb.Status.CompletionTime
	case chiv1.OperationPhaseRunning:
		exec.State = backupv1alpha1.BackupStateRunning
		exec.StartedAt = chb.Status.StartTime
	case chiv1.OperationPhaseFailed:
		exec.State = backupv1alpha1.BackupStateFailed
		exec.StartedAt = chb.Status.StartTime
		exec.CompletedAt = chb.Status.CompletionTime
		exec.Message = backupFailureMessage(&chb.Status)
	case chiv1.OperationPhasePending:
		exec.State = backupv1alpha1.BackupStatePending
	default:
		exec.State = backupv1alpha1.BackupStatePending
	}

	return exec, nil
}

// SyncRestore reconciles a single Restore CR.
func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	return controller.RestoreExecutionStatus{
		State:   backupv1alpha1.RestoreStateFailed,
		Message: "restore is not supported yet by this provider; use a ClickHouseRestore directly",
	}, nil
}

// CleanupBackup deletes the operator ClickHouseBackup resource and waits until gone.
func (p *Provider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up backup", "name", backup.Name)

	if backup.Namespace == "" {
		backup.Namespace = c.Namespace()
	}

	if shouldDeleteBackupData(backup) {
		done, missingInstance, err := p.deleteBackupData(c, backup)
		if err != nil {
			return false, err
		}
		if missingInstance {
			chb := &chiv1.ClickHouseBackup{}
			if getErr := c.Get(chb, backup.Name); getErr == nil && chb.DeletionTimestamp.IsZero() {
				_ = c.Delete(chb)
			}
			return true, nil
		}
		if !done {
			return false, nil
		}
	}

	chb := &chiv1.ClickHouseBackup{}
	if err := c.Get(chb, backup.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get ClickHouseBackup: %w", err)
	}

	if chb.DeletionTimestamp.IsZero() {
		if err := c.Delete(chb); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete ClickHouseBackup: %w", err)
		}
	}
	return false, nil
}

// CleanupRestore removes restore resources.
func (p *Provider) CleanupRestore(c *controller.Context, restore *backupv1alpha1.Restore) (bool, error) {
	return true, nil
}

// BackupWatches implements controller.BackupWatcher.
func (p *Provider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchOwned(&chiv1.ClickHouseBackup{}),
	}
}
