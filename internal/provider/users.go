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
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"strconv"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// The credentials Secret (<instance>-credentials) is the source of truth for
// every ClickHouse credential of an Instance:
//
//   - username / password: the administrative user. Seeded from
//     spec.userSecretRef or generated; editing password rotates it.
//   - password_sha256_hex: derived from password, referenced by the CHI so the
//     plaintext never reaches the ClickHouse configuration.
//   - default_password(_sha256_hex): the built-in "default" user, which the
//     operator leaves password-less and reachable from every ClickHouse pod.
//   - interserver_password: replication fetches (interserver_http_credentials).
//   - cluster_secret: Distributed and ON CLUSTER queries, which then run as
//     the initiating user instead of "default".
//
// The generated credentials are created once and kept. The operator injects
// all of them as environment variables, so the pods roll whenever they change:
// the Secret counts changes in revision (any credential) and user_revision
// (the administrative user's, for the rollout gate).

// ensureCredentials creates or completes the credentials Secret and returns it.
func ensureCredentials(c *controller.Context) (*corev1.Secret, error) {
	name := credentialsSecretName(c.Name())

	existing := &corev1.Secret{}
	data := map[string][]byte{}
	switch err := c.Get(existing, name); {
	case err == nil:
		maps.Copy(data, existing.Data)
	case apierrors.IsNotFound(err):
		seed, err := seedCredentials(c)
		if err != nil {
			return nil, err
		}
		maps.Copy(data, seed)
	default:
		return nil, fmt.Errorf("read credentials secret: %w", err)
	}

	if len(data[common.CredentialsKeyUsername]) == 0 {
		data[common.CredentialsKeyUsername] = []byte(common.AppUserName)
	}
	if len(data[common.CredentialsKeyPassword]) == 0 {
		return nil, fmt.Errorf("credentials secret %s has no %q", name, common.CredentialsKeyPassword)
	}
	changed := false
	for _, key := range []string{
		common.CredentialsKeyDefaultPassword,
		common.CredentialsKeyInterserverPassword,
		common.CredentialsKeyClusterSecret,
	} {
		if len(data[key]) == 0 {
			generated, err := generatePassword(common.AppUserPasswordBytes)
			if err != nil {
				return nil, fmt.Errorf("generate %s: %w", key, err)
			}
			data[key] = []byte(generated)
			changed = true
		}
	}

	// Derived on every reconcile, so editing a password rotates it.
	// ClickHouse's password_sha256_hex form mandates SHA256; the digests stay
	// in this Secret, referenced by the CHI, never copied out of it.
	userChanged := false
	if digest := sha256Hex(data[common.CredentialsKeyPassword]); !bytes.Equal(digest, data[common.CredentialsKeyPasswordSHA256]) {
		data[common.CredentialsKeyPasswordSHA256] = digest
		userChanged = true
	}
	if !bytes.Equal(data[common.CredentialsKeyUsername], data[common.CredentialsKeyAppliedUsername]) {
		data[common.CredentialsKeyAppliedUsername] = data[common.CredentialsKeyUsername]
		userChanged = true
	}
	if digest := sha256Hex(data[common.CredentialsKeyDefaultPassword]); !bytes.Equal(digest, data[common.CredentialsKeyDefaultPasswordSHA256]) {
		data[common.CredentialsKeyDefaultPasswordSHA256] = digest
		changed = true
	}
	if userChanged {
		bumpRevision(data, common.CredentialsKeyUserRevision)
	}
	if changed || userChanged {
		bumpRevision(data, common.CredentialsKeyRevision)
	}

	if existing.Name != "" && maps.EqualFunc(existing.Data, data, bytes.Equal) {
		return existing, nil
	}
	secret := &corev1.Secret{ObjectMeta: c.ObjectMeta(name), Type: corev1.SecretTypeOpaque, Data: data}
	if err := c.Apply(secret); err != nil {
		return nil, fmt.Errorf("apply credentials secret: %w", err)
	}
	log.FromContext(c.Context()).Info("Updated ClickHouse credentials secret", "name", name)
	return secret, nil
}

// seedCredentials returns the initial administrative credentials: those of
// spec.userSecretRef, or a generated password.
func seedCredentials(c *controller.Context) (map[string][]byte, error) {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		password, err := generatePassword(common.AppUserPasswordBytes)
		if err != nil {
			return nil, fmt.Errorf("generate password: %w", err)
		}
		return map[string][]byte{common.CredentialsKeyPassword: []byte(password)}, nil
	}

	src := &corev1.Secret{}
	if err := c.Get(src, ref.Name); err != nil {
		return nil, fmt.Errorf("read spec.userSecretRef secret %s: %w", ref.Name, err)
	}
	if err := validateUserSecretData(src.Data); err != nil {
		return nil, fmt.Errorf("spec.userSecretRef secret %s: %w", ref.Name, err)
	}
	return map[string][]byte{
		common.CredentialsKeyUsername: src.Data[common.CredentialsKeyUsername],
		common.CredentialsKeyPassword: src.Data[common.CredentialsKeyPassword],
	}, nil
}

// usernamePattern matches the user names ClickHouse accepts unquoted in its
// configuration.
var usernamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// validateUserSecretData checks administrative credentials, from
// spec.userSecretRef or an edited credentials Secret.
func validateUserSecretData(data map[string][]byte) error {
	if username := string(data[common.CredentialsKeyUsername]); username != "" {
		if !usernamePattern.MatchString(username) {
			return fmt.Errorf("username %q must start with a letter or underscore and contain only letters, digits and underscores", username)
		}
		switch username {
		case "default", common.InterserverUser, "clickhouse_operator":
			return fmt.Errorf("username %q is reserved", username)
		}
	}
	if len(data[common.CredentialsKeyPassword]) < common.MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", common.MinPasswordLength)
	}
	return nil
}

// credentialsSecretName returns the Secret name holding the credentials.
func credentialsSecretName(instanceName string) string {
	return instanceName + common.CredentialsSecretSuffix
}

// buildUserSettings renders the CHI users configuration. Passwords are
// referenced by their SHA256 digests in the credentials Secret, so no
// plaintext lands in the ClickHouse configuration.
//
// The administrative user accepts any network (::/0): the password is the
// access gate, as with typical managed databases; it may manage users,
// profiles and quotas through SQL (access_management).
func buildUserSettings(secretName, username string) *chiv1.Settings {
	users := chiv1.NewSettings()
	users.Set(username+"/password_sha256_hex", secretSetting(secretName, common.CredentialsKeyPasswordSHA256))
	users.Set(username+"/networks/ip", chiv1.NewSettingVector([]string{"::/0"}))
	users.Set(username+"/profile", chiv1.NewSettingScalar("default"))
	users.Set(username+"/quota", chiv1.NewSettingScalar("default"))
	users.Set(username+"/access_management", chiv1.NewSettingScalar("1"))
	users.Set(username+"/named_collection_control", chiv1.NewSettingScalar("1"))
	users.Set("default/password_sha256_hex", secretSetting(secretName, common.CredentialsKeyDefaultPasswordSHA256))
	return users
}

// applyInterserverAuth makes replicas authenticate each other: replication
// fetches with interserver_http_credentials, Distributed and ON CLUSTER
// queries with the cluster secret (they then run as the initiating user).
func applyInterserverAuth(settings *chiv1.Settings, cluster *chiv1.Cluster, secretName string) {
	settings.Set("interserver_http_credentials/user", chiv1.NewSettingScalar(common.InterserverUser))
	settings.Set("interserver_http_credentials/password", secretSetting(secretName, common.CredentialsKeyInterserverPassword))
	cluster.Secret = &chiv1.ClusterSecret{ValueFrom: &chtypes.DataSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  common.CredentialsKeyClusterSecret,
		},
	}}
}

func secretSetting(secretName, key string) *chiv1.Setting {
	return chiv1.NewSettingSource(&chiv1.SettingSource{
		ValueFrom: &chtypes.DataSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	})
}

// bumpRevision increments the counter stored under key.
func bumpRevision(data map[string][]byte, key string) {
	n, _ := strconv.Atoi(string(data[key]))
	data[key] = []byte(strconv.Itoa(n + 1))
}

// credentialsRevision counts changes to any credential the pods read at
// start-up; the ClickHouse pod template carries it to roll on rotation.
func credentialsRevision(secret *corev1.Secret) string {
	return string(secret.Data[common.CredentialsKeyRevision])
}

// userCredentialsRevision counts changes to the administrative credentials,
// so the rollout gate treats rotating them as a user change.
func userCredentialsRevision(secret *corev1.Secret) string {
	return string(secret.Data[common.CredentialsKeyUserRevision])
}

func sha256Hex(value []byte) []byte {
	sum := sha256.Sum256(value)
	return []byte(hex.EncodeToString(sum[:]))
}

// generatePassword returns a hex-encoded random password with n bytes of entropy.
func generatePassword(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
