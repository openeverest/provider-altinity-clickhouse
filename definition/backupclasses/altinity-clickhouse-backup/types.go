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

// Package altinityclickhousebackup contains the schema-bearing Go types of
// the "altinity-clickhouse-backup" BackupClass. Each struct is converted to an
// OpenAPI v3 schema by `provider-sdk generate` and inlined into the generated
// BackupClass manifest.
//
// +k8s:openapi-gen=true
package altinityclickhousebackup

// BackupParameters are the parameters of a Backup (spec.parameters).
type BackupParameters struct {
	// ReplicaSelection picks the replicas backed up: FirstPerShard (default)
	// backs up one replica of each shard, which is complete for Replicated*
	// tables; AllReplicas backs up every replica, needed for non-replicated
	// tables whose data differs between replicas.
	// +kubebuilder:validation:Enum=FirstPerShard;AllReplicas
	ReplicaSelection string `json:"replicaSelection,omitempty"`
}

// RestoreParameters are the parameters of a Restore (spec.parameters).
type RestoreParameters struct {
	// Overwrite allows restoring over existing tables, which are dropped
	// first. Without it a restore into an Instance that already holds the
	// backup's tables is refused.
	Overwrite bool `json:"overwrite,omitempty"`
}
