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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func testBackupStorage(name, secretName, bucket, endpoint, region string, forcePathStyle, verifyTLS *bool) *backupv1alpha1.BackupStorage {
	return &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: backupv1alpha1.BackupStorageSpec{
			Type: backupv1alpha1.BackupStorageTypeS3,
			S3: &backupv1alpha1.BackupStorageS3Spec{
				Bucket:                  bucket,
				EndpointURL:             endpoint,
				Region:                  region,
				ForcePathStyle:          forcePathStyle,
				VerifyTLS:               verifyTLS,
				CredentialsSecretRef:    commonv1alpha1.SecretRef{Name: secretName},
			},
		},
	}
}

func withBackup(instance *corev1alpha1.Instance, enabled bool, storages ...corev1alpha1.InstanceBackupStorage) *corev1alpha1.Instance {
	out := instance.DeepCopy()
	out.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
		Enabled:  enabled,
		Storages: storages,
	}
	return out
}

func backupPolicyIn(t *testing.T, c *controller.Context) *networkingv1.NetworkPolicy {
	t.Helper()
	policy := &networkingv1.NetworkPolicy{}
	found, err := getOptional(c, policy, c.Name()+common.BackupAPIPolicySuffix)
	require.NoError(t, err)
	if !found {
		return nil
	}
	return policy
}

func storageSecretIn(t *testing.T, c *controller.Context) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	found, err := getOptional(c, secret, backupStorageSecretName(c.Name()))
	require.NoError(t, err)
	if !found {
		return nil
	}
	return secret
}

func autoBackupSecretIn(t *testing.T, c *controller.Context) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	found, err := getOptional(c, secret, autoBackupSecretName(c.Name()))
	require.NoError(t, err)
	if !found {
		return nil
	}
	return secret
}

func boolPtr(b bool) *bool { return &b }

func TestBackupDisabled(t *testing.T) {
	instanceNilBackup := newTestInstance("db", "ns", "")
	instanceDisabledBackup := withBackup(newTestInstance("db", "ns", ""), false, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "bs-1"},
	})

	cNil := newTestContext(t, instanceNilBackup)
	cDisabled := newTestContext(t, instanceDisabledBackup)

	// inputsHash equal for Instance with spec.backup nil vs backup disabled
	hashNil, err := inputsHash(cNil)
	require.NoError(t, err)
	hashDisabled, err := inputsHash(cDisabled)
	require.NoError(t, err)
	assert.Equal(t, hashNil, hashDisabled, "inputsHash must be identical when backup is disabled")

	// ensureBackupInfra is a no-op when backup is disabled
	require.NoError(t, ensureBackupInfra(cDisabled))
	assert.Nil(t, storageSecretIn(t, cDisabled), "no storage secret created when backup disabled")
	assert.Nil(t, autoBackupSecretIn(t, cDisabled), "no auto-backup secret created when backup disabled")

	// buildCHIWired renders CHI spec.Backup as nil
	chi, err := buildCHI(cDisabled, 1)
	require.NoError(t, err)
	assert.Nil(t, chi.Spec.Backup, "CHI spec.Backup must be nil when backup disabled")

	// NetworkPolicy -backup-api is not created
	require.NoError(t, reconcilePlaintextPolicy(cDisabled, completedCHI()))
	assert.Nil(t, backupPolicyIn(t, cDisabled), "-backup-api policy must not exist when backup disabled")
}

func TestBackupEnabled_ConfigYMLAndCHISpec(t *testing.T) {
	bs := testBackupStorage("s3-storage", "s3-creds", "my-bucket", "https://s3.example.com", "us-east-1", boolPtr(true), boolPtr(false))
	creds := secretWith("s3-creds", map[string]string{
		"AWS_ACCESS_KEY_ID":     "test-access-key",
		"AWS_SECRET_ACCESS_KEY": "test-secret-key",
	})
	instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage"},
	})

	c := newTestContext(t, instance, bs, creds)

	require.NoError(t, ensureBackupInfra(c))

	// Storage secret created and config.yml parses back to expected values
	storageSecret := storageSecretIn(t, c)
	require.NotNil(t, storageSecret)
	assert.Equal(t, backupStorageSecretName("db"), storageSecret.Name)

	rawConfig := storageSecret.Data[BackupStorageConfigKey]
	require.NotEmpty(t, rawConfig)

	var parsed backupStorageConfigFile
	require.NoError(t, yaml.Unmarshal(rawConfig, &parsed))

	assert.Equal(t, "s3", parsed.General.RemoteStorage)
	assert.Equal(t, "https://s3.example.com", parsed.S3.Endpoint)
	assert.Equal(t, "us-east-1", parsed.S3.Region)
	assert.Equal(t, "my-bucket", parsed.S3.Bucket)
	assert.Equal(t, "test-access-key", parsed.S3.AccessKey)
	assert.Equal(t, "test-secret-key", parsed.S3.SecretKey)
	assert.True(t, parsed.S3.ForcePathStyle)
	assert.True(t, parsed.S3.DisableCertVerification, "disable_cert_verification should be true when VerifyTLS is false")

	// CHI spec.backup names the Secret and is enabled
	chi, err := buildCHI(c, 1)
	require.NoError(t, err)
	require.NotNil(t, chi.Spec.Backup)
	assert.True(t, chi.Spec.Backup.Enabled.IsTrue())
	require.NotNil(t, chi.Spec.Backup.ConfigSecretRef)
	assert.Equal(t, backupStorageSecretName("db"), chi.Spec.Backup.ConfigSecretRef.Name)
}

func TestEnsureBackupInfra_ErrorCases(t *testing.T) {
	secretVal := "super-secret-key-12345"

	t.Run("missing BackupStorage gives WaitError", func(t *testing.T) {
		instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "nonexistent"},
		})
		c := newTestContext(t, instance)
		err := ensureBackupInfra(c)
		require.Error(t, err)
		var waitErr *controller.WaitError
		require.ErrorAs(t, err, &waitErr)
		assert.Contains(t, waitErr.Reason, "not yet present")
	})

	t.Run("missing credentials Secret gives WaitError", func(t *testing.T) {
		bs := testBackupStorage("bs-missing-creds", "missing-secret", "my-bucket", "https://s3.example.com", "us-east-1", nil, nil)
		instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "bs-missing-creds"},
		})
		c := newTestContext(t, instance, bs)
		err := ensureBackupInfra(c)
		require.Error(t, err)
		var waitErr *controller.WaitError
		require.ErrorAs(t, err, &waitErr)
		assert.Contains(t, waitErr.Reason, "not yet present")
	})

	t.Run("non-S3 storage gives UnsupportedStorage BackupConfigError", func(t *testing.T) {
		nonS3 := &backupv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "non-s3", Namespace: "ns"},
			Spec: backupv1alpha1.BackupStorageSpec{
				Type: "gcs",
			},
		}
		instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "non-s3"},
		})
		c := newTestContext(t, instance, nonS3)
		err := ensureBackupInfra(c)
		require.Error(t, err)
		bce := controller.AsBackupConfigError(err)
		require.NotNil(t, bce)
		assert.Equal(t, "UnsupportedStorage", bce.Reason)
	})

	t.Run("empty keys give CredentialsIncomplete BackupConfigError without credentials in text", func(t *testing.T) {
		bs := testBackupStorage("bs-empty-keys", "incomplete-creds", "my-bucket", "https://s3.example.com", "us-east-1", nil, nil)
		creds := secretWith("incomplete-creds", map[string]string{
			"AWS_ACCESS_KEY_ID":     "",
			"AWS_SECRET_ACCESS_KEY": secretVal,
		})
		instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
			StorageRef: commonv1alpha1.ObjectRef{Name: "bs-empty-keys"},
		})
		c := newTestContext(t, instance, bs, creds)
		err := ensureBackupInfra(c)
		require.Error(t, err)
		bce := controller.AsBackupConfigError(err)
		require.NotNil(t, bce)
		assert.Equal(t, "CredentialsIncomplete", bce.Reason)
		assert.NotContains(t, err.Error(), secretVal, "error text must never contain credential values")
	})
}

func TestAutoBackupSecret_PreservesExistingKeys(t *testing.T) {
	bs := testBackupStorage("s3-storage", "s3-creds", "my-bucket", "https://s3.example.com", "us-east-1", nil, nil)
	creds := secretWith("s3-creds", map[string]string{
		"AWS_ACCESS_KEY_ID":     "key",
		"AWS_SECRET_ACCESS_KEY": "secret",
	})
	instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage"},
	})

	existingAutoSecret := secretWith(autoBackupSecretName("db"), map[string]string{
		"API_USERNAME":            "existing-api-user",
		"API_PASSWORD":            "existing-api-pass",
		BackupSecretKeyPassword:   "existing-ch-pass",
	})

	c := newTestContext(t, instance, bs, creds, existingAutoSecret)

	// First ensureBackupInfra call
	require.NoError(t, ensureBackupInfra(c))

	autoSecret := autoBackupSecretIn(t, c)
	require.NotNil(t, autoSecret)
	assert.Equal(t, "existing-api-user", string(autoSecret.Data["API_USERNAME"]))
	assert.Equal(t, "existing-api-pass", string(autoSecret.Data["API_PASSWORD"]))
	assert.Equal(t, "existing-ch-pass", string(autoSecret.Data[BackupSecretKeyPassword]))
	assert.Equal(t, BackupUsername, string(autoSecret.Data[BackupSecretKeyUsername]))

	// Second ensureBackupInfra call: keeps all unchanged
	require.NoError(t, ensureBackupInfra(c))

	autoSecret2 := autoBackupSecretIn(t, c)
	require.NotNil(t, autoSecret2)
	assert.Equal(t, "existing-api-user", string(autoSecret2.Data["API_USERNAME"]))
	assert.Equal(t, "existing-api-pass", string(autoSecret2.Data["API_PASSWORD"]))
	assert.Equal(t, "existing-ch-pass", string(autoSecret2.Data[BackupSecretKeyPassword]))
	assert.Equal(t, BackupUsername, string(autoSecret2.Data[BackupSecretKeyUsername]))
}

func TestAutoBackupSecret_CreatedWhenAbsent(t *testing.T) {
	bs := testBackupStorage("s3-storage", "s3-creds", "my-bucket", "https://s3.example.com", "us-east-1", nil, nil)
	creds := secretWith("s3-creds", map[string]string{
		"AWS_ACCESS_KEY_ID":     "key",
		"AWS_SECRET_ACCESS_KEY": "secret",
	})
	instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage"},
	})

	c := newTestContext(t, instance, bs, creds)

	require.NoError(t, ensureBackupInfra(c))

	autoSecret := autoBackupSecretIn(t, c)
	require.NotNil(t, autoSecret)
	assert.Equal(t, BackupUsername, string(autoSecret.Data[BackupSecretKeyUsername]))
	assert.Len(t, autoSecret.Data[BackupSecretKeyPassword], 2*common.AppUserPasswordBytes)

	owner := metav1.GetControllerOf(autoSecret)
	require.NotNil(t, owner, "auto-backup-secret must have a controller owner reference")
	assert.Equal(t, instance.Name, owner.Name)
	assert.Equal(t, "Instance", owner.Kind)
	assert.True(t, *owner.Controller)
}

func TestBackupAPINetworkPolicy(t *testing.T) {
	t.Setenv(common.EnvOperatorNamespace, "clickhouse-provider")
	t.Setenv(common.EnvOperatorPodLabels, "app.kubernetes.io/name=operator,app.kubernetes.io/instance=rel")

	bs := testBackupStorage("s3-storage", "s3-creds", "my-bucket", "https://s3.example.com", "us-east-1", nil, nil)
	creds := secretWith("s3-creds", map[string]string{
		"AWS_ACCESS_KEY_ID":     "key",
		"AWS_SECRET_ACCESS_KEY": "secret",
	})
	instance := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage"},
	})

	c := newTestContext(t, instance, bs, creds)

	// Case 1: TLS not required -> Rule A and Rule B present
	chiPlain := completedCHI()
	require.NoError(t, reconcilePlaintextPolicy(c, chiPlain))

	policy := backupPolicyIn(t, c)
	require.NotNil(t, policy)
	assert.Equal(t, map[string]string{common.LabelCHIName: "db"}, policy.Spec.PodSelector.MatchLabels)
	require.Len(t, policy.Spec.Ingress, 2, "must have rule A and rule B when TLS is not required")

	// Rule A: port 7171 only from backup-chi pods
	ruleA := policy.Spec.Ingress[0]
	assert.Equal(t, []int32{int32(common.BackupAPIPort)}, policyPorts(ruleA.Ports))
	require.Len(t, ruleA.From, 1)
	assert.Equal(t, map[string]string{common.LabelBackupCHIName: "db"}, ruleA.From[0].PodSelector.MatchLabels)

	// Rule B: all other ports open from anywhere (From is empty)
	ruleB := policy.Spec.Ingress[1]
	assert.Empty(t, ruleB.From, "rule B must be open from anywhere")
	expectedPorts := []int32{
		common.HTTPPort,
		common.HTTPSPort,
		common.TCPPort,
		common.TCPSecurePort,
		common.InterserverHTTPPort,
		common.InterserverHTTPSPort,
		common.MetricsPort,
	}
	assert.Equal(t, expectedPorts, policyPorts(ruleB.Ports))

	// Case 2: TLS required -> Rule B absent, only Rule A present
	chiTLS := withTLSMode(completedCHI(), common.TLSModeRequired)
	require.NoError(t, reconcilePlaintextPolicy(c, chiTLS))

	policyTLS := backupPolicyIn(t, c)
	require.NotNil(t, policyTLS)
	require.Len(t, policyTLS.Spec.Ingress, 1, "rule B must be absent when TLS is required")
	assert.Equal(t, []int32{int32(common.BackupAPIPort)}, policyPorts(policyTLS.Spec.Ingress[0].Ports))
	require.Len(t, policyTLS.Spec.Ingress[0].From, 1)
	assert.Equal(t, map[string]string{common.LabelBackupCHIName: "db"}, policyTLS.Spec.Ingress[0].From[0].PodSelector.MatchLabels)

	// Case 3: Disable backup -> policy removed
	instanceDisabled := withBackup(newTestInstance("db", "ns", ""), false, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage"},
	})
	cDisabled := newTestContext(t, instanceDisabled, bs, creds)
	require.NoError(t, reconcilePlaintextPolicy(cDisabled, chiPlain))
	assert.Nil(t, backupPolicyIn(t, cDisabled), "policy must be removed when backup is disabled")
}

func TestInputsHash_BackupChanges(t *testing.T) {
	instNil := newTestInstance("db", "ns", "")
	instDisabled := withBackup(newTestInstance("db", "ns", ""), false, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage-1"},
	})
	instEnabledNoStorage := newTestInstance("db", "ns", "")
	instEnabledNoStorage.Spec.Backup = &corev1alpha1.InstanceBackupSpec{
		Enabled: true,
	}
	instEnabled1 := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage-1"},
	})
	instEnabledSchedule := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage-1"},
		Schedules: []corev1alpha1.InstanceBackupSchedule{
			{Name: "daily", Cron: "0 0 * * *"},
		},
	})
	instEnabled2 := withBackup(newTestInstance("db", "ns", ""), true, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "s3-storage-2"},
	})

	hashNil, err := inputsHash(newTestContext(t, instNil))
	require.NoError(t, err)

	hashDisabled, err := inputsHash(newTestContext(t, instDisabled))
	require.NoError(t, err)
	assert.Equal(t, hashNil, hashDisabled, "disabled backup hash equals nil backup hash")

	hashNoStorage, err := inputsHash(newTestContext(t, instEnabledNoStorage))
	require.NoError(t, err)
	assert.Equal(t, hashNil, hashNoStorage, "enabled with no storage gives the same hash as spec.backup nil")

	hashEnabled1, err := inputsHash(newTestContext(t, instEnabled1))
	require.NoError(t, err)
	assert.NotEqual(t, hashNil, hashEnabled1, "enabling backup must change inputsHash")

	hashSchedule, err := inputsHash(newTestContext(t, instEnabledSchedule))
	require.NoError(t, err)
	assert.Equal(t, hashEnabled1, hashSchedule, "changing schedules only must not change inputsHash")

	hashEnabled2, err := inputsHash(newTestContext(t, instEnabled2))
	require.NoError(t, err)
	assert.NotEqual(t, hashEnabled1, hashEnabled2, "changing storage name must change inputsHash")
}
