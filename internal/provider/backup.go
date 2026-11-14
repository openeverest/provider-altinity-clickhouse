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
	"slices"
	"time"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	apicommon "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// Backups run in the Altinity operator (0.28.0+, see docs/backup.md there):
// the CHI turns on its clickhouse-backup sidecar (spec.backup), configured by
// a Secret the provider renders from the Instance's BackupStorage, and Backup
// and Restore CRs map to ClickHouseBackup and ClickHouseRestore. The operator
// keeps every Instance's backups apart under
// backup/{namespace}/{installation}/{cluster}/{shard} in the bucket.

// backupStorageName returns the BackupStorage the Instance keeps its backups
// in, "" when it has none. It is the only one: the BackupClass allows one
// storage, as clickhouse-backup has one remote storage per cluster. A storage
// turns the sidecar on whether or not scheduled backups are enabled, so that
// on-demand backups and restores (a data source included) work.
func backupStorageName(in *corev1alpha1.Instance) string {
	if in.Spec.Backup == nil || len(in.Spec.Backup.Storages) == 0 {
		return ""
	}
	return in.Spec.Backup.Storages[0].StorageRef.Name
}

func backupConfigSecretName(instanceName string) string {
	return instanceName + common.BackupConfigSecretSuffix
}

// chiBackup is the CHI's spec.backup: the sidecar with the storage Secret
// while the Instance has a backup storage, absent otherwise, so an Instance
// without backups renders exactly as before.
func chiBackup(c *controller.Context) *chiv1.ClusterBackup {
	if backupStorageName(c.Instance()) == "" {
		return nil
	}
	return &chiv1.ClusterBackup{
		Enabled:         chtypes.NewStringBool(true),
		ConfigSecretRef: &corev1.LocalObjectReference{Name: backupConfigSecretName(c.Name())},
	}
}

// s3Config is the s3 section of the clickhouse-backup configuration.
type s3Config struct {
	Endpoint                string `json:"endpoint"`
	Region                  string `json:"region"`
	Bucket                  string `json:"bucket"`
	AccessKey               string `json:"access_key"`
	SecretKey               string `json:"secret_key"`
	ForcePathStyle          bool   `json:"force_path_style"`
	DisableCertVerification bool   `json:"disable_cert_verification"`
}

// backupConfig renders the clickhouse-backup configuration (config.yml) of a
// BackupStorage. The path is left to the operator's default, which keeps
// every Instance apart.
func backupConfig(c *controller.Context, bs *backupv1alpha1.BackupStorage) ([]byte, error) {
	if bs.Spec.Type != backupv1alpha1.BackupStorageTypeS3 || bs.Spec.S3 == nil {
		return nil, fmt.Errorf("BackupStorage %s is of type %q: only s3 is supported", bs.Name, bs.Spec.Type)
	}
	accessKey, secretKey, err := c.BackupStorageCredentials(bs)
	if err != nil {
		return nil, err
	}
	s3 := bs.Spec.S3
	return yaml.Marshal(map[string]any{
		"general": map[string]any{"remote_storage": "s3"},
		"s3": s3Config{
			Endpoint:                s3.EndpointURL,
			Region:                  s3.Region,
			Bucket:                  s3.Bucket,
			AccessKey:               accessKey,
			SecretKey:               secretKey,
			ForcePathStyle:          s3.ForcePathStyle != nil && *s3.ForcePathStyle,
			DisableCertVerification: s3.VerifyTLS != nil && !*s3.VerifyTLS,
		},
	})
}

// reconcileBackupConfig keeps the storage Secret the sidecar reads in step
// with the Instance's BackupStorage. A change of it restarts nothing: the
// operator regenerates the sidecars' configuration file.
func reconcileBackupConfig(c *controller.Context) error {
	name := backupStorageName(c.Instance())
	secret := &corev1.Secret{ObjectMeta: c.ObjectMeta(backupConfigSecretName(c.Name()))}
	if name == "" {
		return c.Delete(secret)
	}
	bs, err := c.BackupStorage(name)
	if err != nil {
		return fmt.Errorf("read BackupStorage %s: %w", name, err)
	}
	config, err := backupConfig(c, bs)
	if err != nil {
		return err
	}
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{common.BackupConfigKey: config}
	if err := c.Apply(secret); err != nil {
		return fmt.Errorf("apply backup storage Secret: %w", err)
	}
	return nil
}

// autoBackupSecretName is the Secret the operator creates for the sidecars of
// a CHI, holding their REST API credentials and, when set, the ClickHouse
// user they log in as.
func autoBackupSecretName(instanceName string) string {
	return instanceName + "-auto-backup-secret"
}

// ensureBackupUser gives the sidecars a ClickHouse user of their own: the
// provider password-protects the default user they would connect as
// otherwise. With both credentials in the operator's Secret, the operator
// adds the user to the CHI, limited to localhost (the sidecar's pod), and
// configures the sidecars with it. Only those two keys are applied, under
// the provider's field manager, and only once the operator has created the
// Secret: it recreates one that lacks its REST API keys. The password lives
// there alone; if the operator recreates the Secret, a new one is set.
func ensureBackupUser(c *controller.Context) error {
	name := autoBackupSecretName(c.Name())
	live := &corev1.Secret{}
	found, err := getOptional(c, live, name)
	if err != nil || !found {
		return err
	}
	if string(live.Data[common.BackupUserKeyUsername]) == common.BackupUserName && len(live.Data[common.BackupUserKeyPassword]) > 0 {
		return nil
	}
	password, err := generatePassword(common.AppUserPasswordBytes)
	if err != nil {
		return fmt.Errorf("generate the backup user's password: %w", err)
	}
	apply := corev1ac.Secret(name, c.Namespace()).WithData(map[string][]byte{
		common.BackupUserKeyUsername: []byte(common.BackupUserName),
		common.BackupUserKeyPassword: []byte(password),
	})
	if err := c.Client().Apply(c.Context(), apply, client.FieldOwner(common.BackupUserFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("set the backup user in %s: %w", name, err)
	}
	return nil
}

// backupSidecarReady reports whether the operator has rolled the sidecar out
// to the ClickHouse pods with its ClickHouse user, so backups and restores
// can run.
func backupSidecarReady(c *controller.Context) (bool, error) {
	chi := &chiv1.ClickHouseInstallation{}
	found, err := getOptional(c, chi, c.Name())
	if err != nil || !found {
		return false, err
	}
	if !chi.Spec.Backup.IsEnabled() || chiInstallation(chi).state != chiv1.StatusCompleted {
		return false, nil
	}
	secret := &corev1.Secret{}
	if found, err := getOptional(c, secret, autoBackupSecretName(c.Name())); err != nil || !found {
		return false, err
	}
	return len(secret.Data[common.BackupUserKeyPassword]) > 0, nil
}

// SyncBackup implements controller.BackupProvider: a ClickHouseBackup owned
// by the Backup, of every cluster of the Instance.
func (p *Provider) SyncBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	storage := backupStorageName(c.Instance())
	switch {
	case storage == "":
		return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStateFailed,
			Message: "the Instance keeps no backups: add a storage to spec.backup.storages"}, nil
	case backup.Spec.StorageRef.Name != storage:
		return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStateFailed,
			Message: fmt.Sprintf("the Instance keeps its backups in BackupStorage %s", storage)}, nil
	}

	chb := &chiv1.ClickHouseBackup{ObjectMeta: metav1.ObjectMeta{Name: backup.Name, Namespace: backup.Namespace}}
	found, err := getOptional(c, chb, backup.Name)
	if err != nil {
		return controller.BackupExecutionStatus{}, err
	}
	if !found {
		if ready, err := backupSidecarReady(c); err != nil || !ready {
			return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStatePending,
				Message: "waiting for the backup sidecar on the ClickHouse pods"}, err
		}
	}
	var params struct {
		ReplicaSelection string `json:"replicaSelection,omitempty"`
	}
	if backup.Spec.Parameters != nil && len(backup.Spec.Parameters.Raw) > 0 {
		if err := json.Unmarshal(backup.Spec.Parameters.Raw, &params); err != nil {
			return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStateFailed,
				Message: "invalid parameters: " + err.Error()}, nil
		}
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), chb, func() error {
		// clickHouseInstallation is immutable; a new resource sets it once.
		chb.Spec.ClickHouseInstallation = c.Name()
		if !found && params.ReplicaSelection != "" {
			chb.Spec.ReplicaSelection = chiv1.ReplicaSelection(params.ReplicaSelection)
		}
		if chb.Labels == nil {
			chb.Labels = map[string]string{}
		}
		chb.Labels[controller.InstanceLabel] = c.Name()
		return controllerutil.SetControllerReference(backup, chb, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create ClickHouseBackup: %w", err)
	}

	status := controller.BackupExecutionStatus{
		OperatorBackupRef: &apicommon.TypedObjectRef{Group: chiv1.SchemeGroupVersion.Group, Kind: "ClickHouseBackup", Name: chb.Name},
	}
	status.StartedAt, status.CompletedAt = chb.Status.StartTime, chb.Status.CompletionTime
	switch chb.Status.Phase {
	case chiv1.OperationPhaseCompleted:
		status.State = backupv1alpha1.BackupStateSucceeded
		status.Message = "backup " + chb.Status.BackupName
	case chiv1.OperationPhaseFailed:
		status.State = backupv1alpha1.BackupStateFailed
		status.Message = operationFailure(chb.Status.Clusters, chb.Status.Conditions)
	case chiv1.OperationPhaseRunning:
		status.State = backupv1alpha1.BackupStateRunning
	default:
		status.State = backupv1alpha1.BackupStatePending
	}
	return status, nil
}

// operationFailure explains why an operator backup or restore failed: the
// message of its failed clusters, else of its last False condition.
func operationFailure(clusters []chiv1.OperationClusterStatus, conditions []metav1.Condition) string {
	for _, cluster := range clusters {
		if cluster.Phase == chiv1.OperationPhaseFailed && cluster.Message != "" {
			return fmt.Sprintf("cluster %s: %s", cluster.Name, cluster.Message)
		}
	}
	for _, condition := range slices.Backward(conditions) {
		if condition.Status == metav1.ConditionFalse && condition.Message != "" {
			return condition.Message
		}
	}
	return "the operator reports the operation failed"
}

// CleanupBackup implements controller.BackupProvider. Deleting the
// ClickHouseBackup removes its Jobs; the backup stays in the storage.
func (p *Provider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	chb := &chiv1.ClickHouseBackup{ObjectMeta: metav1.ObjectMeta{Name: backup.Name, Namespace: backup.Namespace}}
	if err := c.Delete(chb); err != nil {
		return false, fmt.Errorf("delete ClickHouseBackup: %w", err)
	}
	found, err := getOptional(c, &chiv1.ClickHouseBackup{}, backup.Name)
	return !found, err
}

// restoreSource resolves the operator backup a Restore reads: its name and
// the Instance that made it.
func restoreSource(c *controller.Context, restore *backupv1alpha1.Restore) (name, instance, storage string, err error) {
	ds := restore.Spec.DataSource
	if ds.Type != backupv1alpha1.DataSourceTypeBackup || ds.Backup == nil {
		return "", "", "", fmt.Errorf("only restores from a backup are supported, not %q", ds.Type)
	}
	backup := &backupv1alpha1.Backup{}
	if err := c.Get(backup, ds.Backup.BackupRef.Name); err != nil {
		return "", "", "", fmt.Errorf("read Backup %s: %w", ds.Backup.BackupRef.Name, err)
	}
	if backup.Status.OperatorBackupRef == nil || backup.Status.State != backupv1alpha1.BackupStateSucceeded {
		return "", "", "", fmt.Errorf("backup %s has not succeeded", backup.Name)
	}
	chb := &chiv1.ClickHouseBackup{}
	if err := c.Get(chb, backup.Status.OperatorBackupRef.Name); err != nil {
		return "", "", "", fmt.Errorf("read ClickHouseBackup %s: %w", backup.Status.OperatorBackupRef.Name, err)
	}
	if chb.Status.BackupName == "" {
		return "", "", "", fmt.Errorf("ClickHouseBackup %s names no backup", chb.Name)
	}
	if backup.Spec.Origin.InstanceRef != nil {
		instance = backup.Spec.Origin.InstanceRef.Name
	}
	return chb.Status.BackupName, instance, backup.Spec.StorageRef.Name, nil
}

// SyncRestore implements controller.BackupProvider: a ClickHouseRestore owned
// by the Restore. It serves restores into a running Instance and the one the
// runtime creates for an Instance's data source.
func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	chr := &chiv1.ClickHouseRestore{ObjectMeta: metav1.ObjectMeta{Name: restore.Name, Namespace: restore.Namespace}}
	found, err := getOptional(c, chr, restore.Name)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if !found {
		backupName, sourceInstance, storage, err := restoreSource(c, restore)
		if err != nil {
			return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateFailed, Message: err.Error()}, nil
		}
		if own := backupStorageName(c.Instance()); own != storage {
			return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateFailed,
				Message: fmt.Sprintf("the backup is in BackupStorage %s: add it to the Instance's spec.backup.storages", storage)}, nil
		}
		if ready, err := backupSidecarReady(c); err != nil || !ready {
			return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStatePending,
				Message: "waiting for the backup sidecar on the ClickHouse pods"}, err
		}
		var params struct {
			Overwrite bool `json:"overwrite,omitempty"`
		}
		if restore.Spec.Parameters != nil && len(restore.Spec.Parameters.Raw) > 0 {
			if err := json.Unmarshal(restore.Spec.Parameters.Raw, &params); err != nil {
				return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateFailed,
					Message: "invalid parameters: " + err.Error()}, nil
			}
		}
		chr.Spec = chiv1.ClickHouseRestoreSpec{ClickHouseInstallation: c.Name(), BackupName: backupName}
		if sourceInstance != "" && sourceInstance != c.Name() {
			chr.Spec.SourceInstallation = sourceInstance
		}
		if params.Overwrite {
			chr.Spec.Overwrite = chtypes.NewStringBool(true)
		}
		chr.Labels = map[string]string{controller.InstanceLabel: c.Name()}
		if err := controllerutil.SetControllerReference(restore, chr, c.Client().Scheme()); err != nil {
			return controller.RestoreExecutionStatus{}, err
		}
		if err := c.Client().Create(c.Context(), chr); err != nil && !apierrors.IsAlreadyExists(err) {
			return controller.RestoreExecutionStatus{}, fmt.Errorf("create ClickHouseRestore: %w", err)
		}
	}

	status := controller.RestoreExecutionStatus{
		OperatorRestoreRef: &apicommon.TypedObjectRef{Group: chiv1.SchemeGroupVersion.Group, Kind: "ClickHouseRestore", Name: chr.Name},
	}
	status.StartedAt, status.CompletedAt = chr.Status.StartTime, chr.Status.CompletionTime
	switch chr.Status.Phase {
	case chiv1.OperationPhaseCompleted:
		status.State = backupv1alpha1.RestoreStateSucceeded
	case chiv1.OperationPhaseFailed:
		status.State = backupv1alpha1.RestoreStateFailed
		status.Message = operationFailure(chr.Status.Clusters, chr.Status.Conditions)
	case chiv1.OperationPhaseRunning:
		status.State = backupv1alpha1.RestoreStateRunning
	default:
		status.State = backupv1alpha1.RestoreStatePending
	}
	return status, nil
}

// CleanupRestore implements controller.BackupProvider.
func (p *Provider) CleanupRestore(c *controller.Context, restore *backupv1alpha1.Restore) (bool, error) {
	chr := &chiv1.ClickHouseRestore{ObjectMeta: metav1.ObjectMeta{Name: restore.Name, Namespace: restore.Namespace}}
	if err := c.Delete(chr); err != nil {
		return false, fmt.Errorf("delete ClickHouseRestore: %w", err)
	}
	found, err := getOptional(c, &chiv1.ClickHouseRestore{}, restore.Name)
	return !found, err
}

// BackupWatches implements controller.BackupWatcher: operator status changes
// reach the owning Backup directly.
func (p *Provider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{controller.WatchOwned(&chiv1.ClickHouseBackup{})}
}

// RestoreWatches implements controller.RestoreWatcher.
func (p *Provider) RestoreWatches() []controller.WatchConfig {
	return []controller.WatchConfig{controller.WatchOwned(&chiv1.ClickHouseRestore{})}
}

func backupScheduleName(instanceName, schedule string) string {
	return instanceName + "-" + schedule
}

// desiredBackupSchedules returns a ClickHouseBackupSchedule for each enabled
// schedule of the Instance's storage while scheduled backups are enabled.
// A count retention keeps that many of the schedule's backups; the operator
// has no time-based retention, so a time retention keeps every backup.
func desiredBackupSchedules(c *controller.Context) []*chiv1.ClickHouseBackupSchedule {
	spec := c.Instance().Spec.Backup
	if backupStorageName(c.Instance()) == "" || !spec.Enabled {
		return nil
	}
	var schedules []*chiv1.ClickHouseBackupSchedule
	for _, s := range spec.Storages[0].Schedules {
		if !s.Enabled {
			continue
		}
		chbs := &chiv1.ClickHouseBackupSchedule{
			TypeMeta:   metav1.TypeMeta{APIVersion: chiv1.SchemeGroupVersion.String(), Kind: "ClickHouseBackupSchedule"},
			ObjectMeta: c.ObjectMeta(backupScheduleName(c.Name(), s.Name)),
			Spec: chiv1.ClickHouseBackupScheduleSpec{
				ClickHouseInstallation: c.Name(),
				Schedule:               s.Cron,
			},
		}
		chbs.Labels[controller.InstanceLabel] = c.Name()
		if r := s.Retention; r != nil && r.Type == corev1alpha1.BackupScheduleRetentionTypeCount && r.Count != nil {
			chbs.Spec.Retention.KeepLast = r.Count
		}
		schedules = append(schedules, chbs)
	}
	return schedules
}

// reconcileBackupSchedules applies the Instance's schedules and deletes the
// ones it no longer has.
func reconcileBackupSchedules(c *controller.Context) error {
	desired := desiredBackupSchedules(c)
	keep := map[string]bool{}
	for _, chbs := range desired {
		keep[chbs.Name] = true
		if err := c.Apply(chbs); err != nil {
			return fmt.Errorf("apply ClickHouseBackupSchedule %s: %w", chbs.Name, err)
		}
	}
	live := &chiv1.ClickHouseBackupScheduleList{}
	if err := c.List(live, client.MatchingLabels{controller.InstanceLabel: c.Name()}); err != nil {
		return fmt.Errorf("list ClickHouseBackupSchedules: %w", err)
	}
	for i := range live.Items {
		if keep[live.Items[i].Name] {
			continue
		}
		if err := c.Delete(&live.Items[i]); err != nil {
			return fmt.Errorf("delete ClickHouseBackupSchedule %s: %w", live.Items[i].Name, err)
		}
	}
	return nil
}

// syncBackups runs once the cluster is up: the schedules, and the restore of
// the Instance's data source, which the runtime drives through a Restore.
func syncBackups(c *controller.Context) error {
	if backupStorageName(c.Instance()) != "" {
		if err := ensureBackupUser(c); err != nil {
			return err
		}
	}
	if err := reconcileBackupSchedules(c); err != nil {
		return err
	}
	if c.Instance().Spec.DataSource == nil {
		return nil
	}
	ds, err := c.ReconcileDataSource()
	if err != nil {
		return fmt.Errorf("reconcile data source: %w", err)
	}
	if !ds.Done {
		return controller.WaitForDuration("restoring the data source: "+ds.Message, 15*time.Second)
	}
	return nil
}

var (
	_ controller.BackupProvider = (*Provider)(nil)
	_ controller.BackupWatcher  = (*Provider)(nil)
	_ controller.RestoreWatcher = (*Provider)(nil)
)
