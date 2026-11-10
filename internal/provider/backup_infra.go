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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

const (
	// BackupUsername is the ClickHouse user the sidecar connects with.
	BackupUsername = "openeverest-backup"

	// BackupStorageConfigKey is the key in the storage Secret holding the
	// clickhouse-backup configuration.
	BackupStorageConfigKey = "config.yml"

	// BackupSecretKeyUsername and BackupSecretKeyPassword are the ClickHouse
	// credential keys inside the <instance>-auto-backup-secret Secret.
	BackupSecretKeyUsername = "CLICKHOUSE_USERNAME"
	BackupSecretKeyPassword = "CLICKHOUSE_PASSWORD"
)

// instanceBackupEnabled reports whether backup is configured in the spec:
// non-nil, Enabled is true, and at least one storage is declared.
func instanceBackupEnabled(b *corev1alpha1.InstanceBackupSpec) bool {
	return b != nil && b.Enabled && len(b.Storages) > 0
}

// backupEnabled reports whether operator-managed backup is configured for the Instance:
// spec.backup is non-nil, Enabled is true, and at least one storage is declared.
func backupEnabled(c *controller.Context) bool {
	return instanceBackupEnabled(c.Instance().Spec.Backup)
}

// backupStorageSecretName returns the name of the Secret holding the
// clickhouse-backup storage configuration.
func backupStorageSecretName(instance string) string {
	return instance + "-backup-storage"
}

// autoBackupSecretName returns the name of the credentials Secret shared between
// the operator and the sidecar.
func autoBackupSecretName(instance string) string {
	return instance + "-auto-backup-secret"
}

type backupStorageConfigFile struct {
	General backupStorageConfigGeneral `json:"general"`
	S3      backupStorageConfigS3      `json:"s3"`
}

type backupStorageConfigGeneral struct {
	RemoteStorage string `json:"remote_storage"`
}

type backupStorageConfigS3 struct {
	Endpoint                string `json:"endpoint"`
	Region                  string `json:"region"`
	Bucket                  string `json:"bucket"`
	AccessKey               string `json:"access_key"`
	SecretKey               string `json:"secret_key"`
	ForcePathStyle          bool   `json:"force_path_style"`
	DisableCertVerification bool   `json:"disable_cert_verification"`
}

// ensureBackupInfra reconciles the storage Secret and auto-backup credentials
// Secret required by the Altinity operator's clickhouse-backup sidecar.
func ensureBackupInfra(c *controller.Context) error {
	if !backupEnabled(c) {
		return nil
	}

	strg := c.Instance().Spec.Backup.Storages[0]
	bg, err := c.BackupStorage(strg.StorageRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.WaitFor(fmt.Sprintf(
				"BackupStorage %q not yet present", strg.StorageRef.Name))
		}
		return &controller.BackupConfigError{
			Reason:  "StorageResolutionFailed",
			Message: err.Error(),
		}
	}
	if bg.Spec.S3 == nil {
		return &controller.BackupConfigError{
			Reason:  "UnsupportedStorage",
			Message: fmt.Sprintf("BackupStorage %q is not S3", bg.Name),
		}
	}

	accessKeyID, secretAccessKey, err := c.BackupStorageCredentials(bg)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controller.WaitFor(fmt.Sprintf(
				"credentials Secret %q for BackupStorage %q not yet present",
				bg.Spec.S3.CredentialsSecretRef.Name, bg.Name))
		}
		return &controller.BackupConfigError{
			Reason:  "CredentialsUnavailable",
			Message: err.Error(),
		}
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return &controller.BackupConfigError{
			Reason: "CredentialsIncomplete",
			Message: fmt.Sprintf(
				"Secret %q must contain AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY",
				bg.Spec.S3.CredentialsSecretRef.Name),
		}
	}

	forcePathStyle := false
	if bg.Spec.S3.ForcePathStyle != nil {
		forcePathStyle = *bg.Spec.S3.ForcePathStyle
	}
	disableCertVerification := false
	if bg.Spec.S3.VerifyTLS != nil {
		disableCertVerification = !*bg.Spec.S3.VerifyTLS
	}

	cfg := backupStorageConfigFile{
		General: backupStorageConfigGeneral{
			RemoteStorage: "s3",
		},
		S3: backupStorageConfigS3{
			Endpoint:                bg.Spec.S3.EndpointURL,
			Region:                  bg.Spec.S3.Region,
			Bucket:                  bg.Spec.S3.Bucket,
			AccessKey:               accessKeyID,
			SecretKey:               secretAccessKey,
			ForcePathStyle:          forcePathStyle,
			DisableCertVerification: disableCertVerification,
		},
	}
	yamlData, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal backup storage config: %w", err)
	}

	storageSecretName := backupStorageSecretName(c.Name())
	storageSecret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(storageSecretName),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			BackupStorageConfigKey: yamlData,
		},
	}
	if err := c.Apply(storageSecret); err != nil {
		return fmt.Errorf("apply backup storage secret %s: %w", storageSecretName, err)
	}

	autoSecretName := autoBackupSecretName(c.Name())
	autoSecret := &corev1.Secret{}
	found, err := getOptional(c, autoSecret, autoSecretName)
	if err != nil {
		return fmt.Errorf("read auto backup secret %s: %w", autoSecretName, err)
	}

	if !found {
		password, err := generatePassword(common.AppUserPasswordBytes)
		if err != nil {
			return fmt.Errorf("generate backup user password: %w", err)
		}
		newSecret := &corev1.Secret{
			ObjectMeta: c.ObjectMeta(autoSecretName),
			Type:       corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				BackupSecretKeyUsername: []byte(BackupUsername),
				BackupSecretKeyPassword: []byte(password),
			},
		}
		if err := controllerutil.SetControllerReference(c.Instance(), newSecret, c.Client().Scheme()); err != nil {
			return fmt.Errorf("set controller reference on auto backup secret %s: %w", autoSecretName, err)
		}
		if err := c.Client().Create(c.Context(), newSecret); err != nil {
			return fmt.Errorf("create auto backup secret %s: %w", autoSecretName, err)
		}
	} else {
		changed := false
		if autoSecret.Data == nil {
			autoSecret.Data = make(map[string][]byte)
		}
		if len(autoSecret.Data[BackupSecretKeyUsername]) == 0 {
			autoSecret.Data[BackupSecretKeyUsername] = []byte(BackupUsername)
			changed = true
		}
		if len(autoSecret.Data[BackupSecretKeyPassword]) == 0 {
			password, err := generatePassword(common.AppUserPasswordBytes)
			if err != nil {
				return fmt.Errorf("generate backup user password: %w", err)
			}
			autoSecret.Data[BackupSecretKeyPassword] = []byte(password)
			changed = true
		}
		if changed {
			if err := c.Client().Update(c.Context(), autoSecret); err != nil {
				return fmt.Errorf("update auto backup secret %s: %w", autoSecretName, err)
			}
		}
	}

	return nil
}
