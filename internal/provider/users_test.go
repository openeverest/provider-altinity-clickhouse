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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func secretWith(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func withUserSecretRef(instance *corev1alpha1.Instance, name string) *corev1alpha1.Instance {
	out := instance.DeepCopy()
	out.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: name}
	return out
}

func TestEnsureCredentialsGeneratesAll(t *testing.T) {
	secret, err := ensureCredentials(newTestContext(t, newTestInstance("db", "ns", "")))
	require.NoError(t, err)

	assert.Equal(t, "admin", string(secret.Data[common.CredentialsKeyUsername]))
	for _, key := range []string{
		common.CredentialsKeyPassword,
		common.CredentialsKeyDefaultPassword,
		common.CredentialsKeyInterserverPassword,
		common.CredentialsKeyClusterSecret,
	} {
		assert.Len(t, secret.Data[key], 2*common.AppUserPasswordBytes, key)
	}
	assert.Equal(t, sha256Hex(secret.Data[common.CredentialsKeyPassword]), secret.Data[common.CredentialsKeyPasswordSHA256])
	assert.Equal(t, sha256Hex(secret.Data[common.CredentialsKeyDefaultPassword]), secret.Data[common.CredentialsKeyDefaultPasswordSHA256])
}

func TestEnsureCredentialsSeedsFromUserSecretRef(t *testing.T) {
	src := secretWith("seed", map[string]string{"username": "dba", "password": "correct-horse-battery"})
	c := newTestContext(t, withUserSecretRef(newTestInstance("db", "ns", ""), "seed"), src)

	secret, err := ensureCredentials(c)
	require.NoError(t, err)
	assert.Equal(t, "dba", string(secret.Data[common.CredentialsKeyUsername]))
	assert.Equal(t, "correct-horse-battery", string(secret.Data[common.CredentialsKeyPassword]))
	assert.NotEmpty(t, secret.Data[common.CredentialsKeyClusterSecret])

	// Seeding happens once: the source Secret no longer matters.
	src.Data["password"] = []byte("another-password-entirely")
	require.NoError(t, c.Client().Update(c.Context(), src))
	again, err := ensureCredentials(c)
	require.NoError(t, err)
	assert.Equal(t, "correct-horse-battery", string(again.Data[common.CredentialsKeyPassword]))
}

func TestEnsureCredentialsRotatesEditedPassword(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))
	first, err := ensureCredentials(c)
	require.NoError(t, err)

	edited := &corev1.Secret{}
	require.NoError(t, c.Get(edited, credentialsSecretName("db")))
	edited.Data[common.CredentialsKeyPassword] = []byte("rotated-password-123")
	require.NoError(t, c.Client().Update(c.Context(), edited))

	rotated, err := ensureCredentials(c)
	require.NoError(t, err)
	assert.Equal(t, sha256Hex([]byte("rotated-password-123")), rotated.Data[common.CredentialsKeyPasswordSHA256])
	for _, key := range []string{common.CredentialsKeyDefaultPassword, common.CredentialsKeyInterserverPassword, common.CredentialsKeyClusterSecret} {
		assert.Equal(t, first.Data[key], rotated.Data[key], "%s is kept", key)
	}
}

func TestEnsureCredentialsCompletesEarlierSecret(t *testing.T) {
	// A Secret written before the default, replication and cluster credentials existed.
	earlier := secretWith(credentialsSecretName("db"), map[string]string{
		"username": "admin", "password": "existing-password-0123", "password_sha256_hex": "stale",
	})
	secret, err := ensureCredentials(newTestContext(t, newTestInstance("db", "ns", ""), earlier))
	require.NoError(t, err)

	assert.Equal(t, "existing-password-0123", string(secret.Data[common.CredentialsKeyPassword]))
	assert.Equal(t, sha256Hex([]byte("existing-password-0123")), secret.Data[common.CredentialsKeyPasswordSHA256])
	assert.NotEmpty(t, secret.Data[common.CredentialsKeyDefaultPassword])
	assert.NotEmpty(t, secret.Data[common.CredentialsKeyInterserverPassword])
	assert.NotEmpty(t, secret.Data[common.CredentialsKeyClusterSecret])
}

func TestValidateCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		data    map[string]string
		wantErr string
	}{
		"valid":             {map[string]string{"password": "long-enough-password"}, ""},
		"custom username":   {map[string]string{"username": "dba_1", "password": "long-enough-password"}, ""},
		"short password":    {map[string]string{"password": "short"}, "at least 12 characters"},
		"missing password":  {map[string]string{"username": "dba"}, "at least 12 characters"},
		"reserved username": {map[string]string{"username": "default", "password": "long-enough-password"}, "reserved"},
		"invalid username":  {map[string]string{"username": "dba-1", "password": "long-enough-password"}, "must start with a letter"},
	} {
		t.Run(name, func(t *testing.T) {
			seeded := withUserSecretRef(newTestInstance("db", "ns", ""), "seed")
			err := validate(t, seeded, secretWith("seed", tc.data))
			// An edited credentials Secret is held to the same rules.
			edited := validate(t, newTestInstance("db", "ns", ""), secretWith(credentialsSecretName("db"), tc.data))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				assert.NoError(t, edited)
				return
			}
			assert.ErrorContains(t, err, tc.wantErr)
			assert.ErrorContains(t, edited, tc.wantErr)
		})
	}
}

func TestBuildCHIHardensAuthentication(t *testing.T) {
	creds := secretWith(credentialsSecretName("db"), map[string]string{
		"username": "dba", "password": "long-enough-password", "password_sha256_hex": "h",
		"default_password_sha256_hex": "d", "interserver_password": "i", "cluster_secret": "s",
	})
	c := newTestContext(t, newTestInstance("db", "ns", ""), creds)

	chi, err := buildCHI(c, 2)
	require.NoError(t, err)

	users := chi.Spec.Configuration.Users
	assert.Equal(t, common.CredentialsKeyPasswordSHA256, users.Get("dba/password_sha256_hex").GetSecretKeyRef().Key)
	assert.False(t, users.Has("admin/password_sha256_hex"), "the custom username replaces admin")
	assert.Equal(t, common.CredentialsKeyDefaultPasswordSHA256, users.Get("default/password_sha256_hex").GetSecretKeyRef().Key)

	settings := chi.Spec.Configuration.Settings
	assert.Equal(t, common.InterserverUser, settings.Get("interserver_http_credentials/user").String())
	assert.Equal(t, common.CredentialsKeyInterserverPassword, settings.Get("interserver_http_credentials/password").GetSecretKeyRef().Key)

	cluster := chi.Spec.Configuration.Clusters[0]
	require.NotNil(t, cluster.Secret)
	assert.Equal(t, common.CredentialsKeyClusterSecret, cluster.Secret.ValueFrom.SecretKeyRef.Key)

	digest := chi.Spec.Templates.PodTemplates[0].ObjectMeta.Annotations[common.AnnotationCredentialsDigest]
	assert.Equal(t, credentialsDigest(creds), digest)
}

func TestValidateRejectsOverridingInterserverCredentials(t *testing.T) {
	err := validate(t, newTestInstance("db", "ns", `{"configuration":"interserver_http_credentials/user: x"}`))
	assert.ErrorContains(t, err, "must not set interserver_http_credentials/user")
}

// Rotating the admin password is a user change: applied (rolling the pods)
// without waiting for maintenance approval.
func TestRolloutAppliesPasswordRotation(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := recordedState(t, instance)

	rotated := withCredentials(nil)[0].(*corev1.Secret)
	rotated.Data["password"] = []byte("rotated-password-123")
	applied, c := syncStandalone(t, instance, completedCHI(), state, rotated)

	require.Len(t, applied, 1)
	assert.Empty(t, c.GetPendingMaintenance())
	digest := applied[0].Spec.Templates.PodTemplates[0].ObjectMeta.Annotations[common.AnnotationCredentialsDigest]
	assert.NotEqual(t, credentialsDigest(withCredentials(nil)[0].(*corev1.Secret)), digest, "the pods roll")
}

// Credentials the provider adds on its own (here: an Instance whose Secret
// predates the default, replication and cluster credentials) roll the pods
// only after maintenance approval.
func TestRolloutHoldsGeneratedCredentials(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	state := recordedState(t, instance)

	earlier := secretWith(credentialsSecretName("db"), map[string]string{
		"username": "admin", "password": "test-password-0123456789",
	})
	applied, c := syncStandalone(t, instance, completedCHI(), state, earlier)

	assert.Empty(t, applied)
	assert.Len(t, c.GetPendingMaintenance(), 1)
}
