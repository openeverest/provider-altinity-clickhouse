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

// Package user contains the schema-bearing Go types for the
// "user" secret type, referenced by an Instance's spec.userSecretRef.
//
// +k8s:openapi-gen=true
package user

// UserSecretData seeds the credentials of the instance's administrative
// ClickHouse user when the instance is created.
type UserSecretData struct {
	// Username of the administrative user. Defaults to "admin".
	Username string `json:"username,omitempty"`

	// Password of the administrative user, at least 12 characters.
	Password string `json:"password"`
}
