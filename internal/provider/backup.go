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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/scaledb-io/provider-altinity-clickhouse/internal/common"
)

// SyncBackup implements controller.BackupProvider. It drives the
// clickhouse-backup sidecar's REST API on this Instance's replica-0 pod (see
// buildBackupContainer for how the sidecar is wired in at Instance creation).
// SyncBackup is idempotent: it only triggers create_remote once per Backup
// name, then polls clickhouse-backup's own status for completion.
func (p *Provider) SyncBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	client := newBackupClient(chiReplicaZeroHost(c))

	entries, err := client.status(c.Context())
	if err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("query clickhouse-backup status: %w", err)
	}
	if entry := findCommand(entries, "create_remote", backup.Name); entry != nil {
		return backupExecutionStatus(*entry), nil
	}

	if err := client.createRemote(c.Context(), backup.Name); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("trigger backup: %w", err)
	}
	now := metav1.NewTime(time.Now())
	return controller.BackupExecutionStatus{State: backupv1alpha1.BackupStatePending, StartedAt: &now}, nil
}

// SyncRestore implements controller.BackupProvider. Restores always target
// this Instance's replica-0 pod; the resolved backup name is the referenced
// Backup CR's name, which SyncBackup used as the clickhouse-backup backup
// name too.
func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	if restore.Spec.DataSource.Type != backupv1alpha1.DataSourceTypeBackup || restore.Spec.DataSource.Backup == nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("unsupported restore data source type %q", restore.Spec.DataSource.Type)
	}
	backupName := restore.Spec.DataSource.Backup.BackupRef.Name

	client := newBackupClient(chiReplicaZeroHost(c))

	entries, err := client.status(c.Context())
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("query clickhouse-backup status: %w", err)
	}
	if entry := findCommand(entries, "restore_remote", backupName); entry != nil {
		return restoreExecutionStatus(*entry), nil
	}

	if err := client.restoreRemote(c.Context(), backupName); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("trigger restore: %w", err)
	}
	now := metav1.NewTime(time.Now())
	return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStatePending, StartedAt: &now}, nil
}

// CleanupBackup implements controller.BackupProvider. Backup data lives in
// remote (S3) storage, not as an in-cluster resource, so cleanup is a single
// best-effort delete call rather than a polled operation.
func (p *Provider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	if c.ShouldRetainBackupData(backup) {
		return true, nil
	}
	client := newBackupClient(chiReplicaZeroHost(c))
	if err := client.deleteRemote(c.Context(), backup.Name); err != nil {
		return false, fmt.Errorf("delete remote backup: %w", err)
	}
	return true, nil
}

// CleanupRestore implements controller.BackupProvider. Restores are
// run-to-completion; there is nothing to clean up.
func (p *Provider) CleanupRestore(_ *controller.Context, _ *backupv1alpha1.Restore) (bool, error) {
	return true, nil
}

// chiReplicaZeroHost returns the stable per-pod DNS name of shard 0 / replica
// 0 for this Instance's CHI, following the same per-pod headless service
// naming convention documented on keeperZookeeperNodes.
func chiReplicaZeroHost(c *controller.Context) string {
	return fmt.Sprintf("chi-%s-%s-0-0.%s.svc", c.Name(), common.CHIClusterName, c.Namespace())
}

// backupExecutionStatus maps a clickhouse-backup status entry onto the
// runtime's BackupExecutionStatus.
func backupExecutionStatus(entry backupStatusEntry) controller.BackupExecutionStatus {
	status := controller.BackupExecutionStatus{
		State:   mapBackupState(entry.Status),
		Message: entry.Error,
	}
	if t, ok := parseBackupTime(entry.Finish); ok {
		status.CompletedAt = &t
	}
	return status
}

// restoreExecutionStatus is the Restore counterpart of backupExecutionStatus.
func restoreExecutionStatus(entry backupStatusEntry) controller.RestoreExecutionStatus {
	status := controller.RestoreExecutionStatus{
		State:   mapRestoreState(entry.Status),
		Message: entry.Error,
	}
	if t, ok := parseBackupTime(entry.Finish); ok {
		status.CompletedAt = &t
	}
	return status
}

func mapBackupState(status string) backupv1alpha1.BackupState {
	switch status {
	case "success":
		return backupv1alpha1.BackupStateSucceeded
	case "error":
		return backupv1alpha1.BackupStateFailed
	default:
		return backupv1alpha1.BackupStateRunning
	}
}

func mapRestoreState(status string) backupv1alpha1.RestoreState {
	switch status {
	case "success":
		return backupv1alpha1.RestoreStateSucceeded
	case "error":
		return backupv1alpha1.RestoreStateFailed
	default:
		return backupv1alpha1.RestoreStateRunning
	}
}

func parseBackupTime(s string) (metav1.Time, bool) {
	if s == "" {
		return metav1.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return metav1.Time{}, false
	}
	return metav1.NewTime(t), true
}

// =============================================================================
// clickhouse-backup REST client
// =============================================================================

// backupClient talks to a clickhouse-backup sidecar's REST API (`clickhouse-backup
// server`, see https://github.com/Altinity/clickhouse-backup/blob/master/Examples.md).
// The API exposes a single, static storage destination fixed via the
// sidecar's env vars at startup, so every call targets one specific pod.
type backupClient struct {
	host       string
	port       int
	httpClient *http.Client
}

func newBackupClient(host string) *backupClient {
	return newBackupClientWithPort(host, backupRESTPort)
}

// newBackupClientWithPort allows tests to point at an httptest.Server's
// random port instead of the fixed backupRESTPort.
func newBackupClientWithPort(host string, port int) *backupClient {
	return &backupClient{
		host:       host,
		port:       port,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (b *backupClient) baseURL() string {
	return fmt.Sprintf("http://%s:%d", b.host, b.port)
}

// backupStatusEntry is one entry of clickhouse-backup's GET /backup/status response.
type backupStatusEntry struct {
	Command string `json:"command"`
	Status  string `json:"status"` // "in progress", "success", "error"
	Start   string `json:"start,omitempty"`
	Finish  string `json:"finish,omitempty"`
	Error   string `json:"error,omitempty"`
}

// createRemote triggers a combined create+upload for the named backup.
func (b *backupClient) createRemote(ctx context.Context, name string) error {
	return b.post(ctx, "/backup/create_remote?name="+url.QueryEscape(name))
}

// restoreRemote triggers a combined download+restore for the named backup.
func (b *backupClient) restoreRemote(ctx context.Context, name string) error {
	return b.post(ctx, "/backup/restore_remote/"+url.PathEscape(name))
}

// deleteRemote removes the named backup from remote storage.
func (b *backupClient) deleteRemote(ctx context.Context, name string) error {
	return b.post(ctx, "/backup/delete/remote/"+url.PathEscape(name))
}

// status returns clickhouse-backup's currently tracked operations.
func (b *backupClient) status(ctx context.Context) ([]backupStatusEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL()+"/backup/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("clickhouse-backup status: %s: %s", resp.Status, string(body))
	}
	var entries []backupStatusEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode clickhouse-backup status: %w", err)
	}
	return entries, nil
}

func (b *backupClient) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL()+path, nil)
	if err != nil {
		return err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("clickhouse-backup %s: %s: %s", path, resp.Status, string(body))
	}
	return nil
}

// findCommand returns the status entry for a given action (e.g.
// "create_remote") and backup name, if present. clickhouse-backup reports the
// full invocation (action + arguments) in the "command" field, so this
// matches on substrings rather than assuming an exact format.
func findCommand(entries []backupStatusEntry, action, name string) *backupStatusEntry {
	for i := range entries {
		if strings.Contains(entries[i].Command, action) && strings.Contains(entries[i].Command, name) {
			return &entries[i]
		}
	}
	return nil
}
