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

// Package provider defines the provider implementation.
// RBAC markers for the Altinity ClickHouse and ClickHouse Keeper operator resources.
package provider

// OpenEverest core resources reconciled by the provider-runtime.
// The provider-runtime reconciler watches Instance CRs, manages their
// finalizers/status, and reads the Provider CR. Without these rules the
// controller's ServiceAccount cannot list Instances at cluster scope and the
// pod fails to reconcile ("instances.core.openeverest.io is forbidden").
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.openeverest.io,resources=instances/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.openeverest.io,resources=providers,verbs=get;list;watch

// Altinity ClickHouseInstallation
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhouseinstallations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhouseinstallations/status,verbs=get
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhouseinstallations/finalizers,verbs=update

// Altinity ClickHouseKeeperInstallation
// +kubebuilder:rbac:groups=clickhouse-keeper.altinity.com,resources=clickhousekeeperinstallations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clickhouse-keeper.altinity.com,resources=clickhousekeeperinstallations/status,verbs=get
// +kubebuilder:rbac:groups=clickhouse-keeper.altinity.com,resources=clickhousekeeperinstallations/finalizers,verbs=update

// Core Kubernetes resources managed by the Altinity operator on our behalf
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch

// Backup/restore (ProviderManaged): the runtime dispatches Backup/Restore CRs
// to SyncBackup/SyncRestore and reads BackupClass/BackupStorage to resolve
// the storage config wired into the clickhouse-backup sidecar.
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=backup.openeverest.io,resources=backupstorages,verbs=get;list;watch
