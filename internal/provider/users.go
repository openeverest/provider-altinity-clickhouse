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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// ensureCredentials creates the application user's credentials Secret if it does
// not already exist and returns it.
//
// The Altinity operator does not provision a usable external user: the default
// user is network-restricted to intra-cluster traffic. We therefore generate a
// dedicated `admin` user whose SHA256 password digest is referenced into the CHI
// users configuration.
//
// The password is generated exactly once. Regenerating it on subsequent
// reconciles would break existing client connections, so we return early when
// the Secret already exists.
func ensureCredentials(c *controller.Context) (*corev1.Secret, error) {
	name := credentialsSecretName(c.Name())

	existing := &corev1.Secret{}
	if err := c.Get(existing, name); err == nil {
		return existing, nil
	}

	password, err := generatePassword(common.AppUserPasswordBytes)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	// ClickHouse's password_sha256_hex form mandates SHA256; the input is a
	// freshly generated cryptographically random secret, not a human password.
	digest := sha256.Sum256([]byte(password))

	secret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(name),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			common.CredentialsKeyUsername:       []byte(common.AppUserName),
			common.CredentialsKeyPassword:       []byte(password),
			common.CredentialsKeyPasswordSHA256: []byte(hex.EncodeToString(digest[:])),
		},
	}
	if err := c.Apply(secret); err != nil {
		return nil, fmt.Errorf("create credentials secret: %w", err)
	}

	log.FromContext(c.Context()).Info("Created ClickHouse credentials secret", "name", name)
	return secret, nil
}

// credentialsSecretName returns the Secret name holding the app user credentials.
func credentialsSecretName(instanceName string) string {
	return instanceName + common.CredentialsSecretSuffix
}

// buildUserSettings renders the CHI users configuration for the provisioned
// `admin` user. The password is referenced by its SHA256 digest from the
// credentials Secret so the plaintext never lands in the ClickHouse config.
//
// networks/ip is open (::/0) because the password is the access gate — this
// matches typical managed-database defaults and lets application pods connect.
func buildUserSettings(secretName string) *chiv1.Settings {
	user := common.AppUserName
	users := chiv1.NewSettings()
	users.Set(user+"/password_sha256_hex", chiv1.NewSettingSource(&chiv1.SettingSource{
		ValueFrom: &chtypes.DataSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  common.CredentialsKeyPasswordSHA256,
			},
		},
	}))
	users.Set(user+"/networks/ip", chiv1.NewSettingVector([]string{"::/0"}))
	users.Set(user+"/profile", chiv1.NewSettingScalar("default"))
	users.Set(user+"/quota", chiv1.NewSettingScalar("default"))
	users.Set(user+"/access_management", chiv1.NewSettingScalar("1"))
	users.Set(user+"/named_collection_control", chiv1.NewSettingScalar("1"))
	return users
}

// generatePassword returns a hex-encoded random password with n bytes of entropy.
func generatePassword(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
