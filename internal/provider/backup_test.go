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
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	apicommon "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

func newTestContext(t *testing.T, in *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register corev1alpha1 scheme: %v", err)
	}
	if err := backupv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register backupv1alpha1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), c, in, "altinity-clickhouse")
}

func TestBuildBackupContainer_Disabled(t *testing.T) {
	tests := []struct {
		name   string
		backup *corev1alpha1.InstanceBackupSpec
	}{
		{name: "nil backup spec", backup: nil},
		{name: "not enabled", backup: &corev1alpha1.InstanceBackupSpec{Enabled: false}},
		{name: "enabled but no storages", backup: &corev1alpha1.InstanceBackupSpec{Enabled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &corev1alpha1.Instance{
				ObjectMeta: metav1.ObjectMeta{Name: "itest", Namespace: "default"},
				Spec:       corev1alpha1.InstanceSpec{Backup: tt.backup},
			}
			c := newTestContext(t, in)
			container, err := buildBackupContainer(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if container != nil {
				t.Fatalf("expected nil container, got %+v", container)
			}
		})
	}
}

func TestBuildBackupContainer_HappyPath(t *testing.T) {
	storage := &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "my-s3-storage", Namespace: "default"},
		Spec: backupv1alpha1.BackupStorageSpec{
			Type: backupv1alpha1.BackupStorageTypeS3,
			S3: &backupv1alpha1.BackupStorageS3Spec{
				Bucket:               "my-bucket",
				Region:               "us-east-1",
				EndpointURL:          "https://s3.amazonaws.com",
				CredentialsSecretRef: apicommon.SecretRef{Name: "my-s3-creds"},
			},
		},
	}
	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "itest", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled: true,
				Storages: []corev1alpha1.InstanceBackupStorage{
					{StorageRef: apicommon.ObjectRef{Name: "my-s3-storage"}},
				},
			},
		},
	}
	c := newTestContext(t, in, storage)

	container, err := buildBackupContainer(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if container == nil {
		t.Fatal("expected a non-nil container")
	}
	if container.Name != backupContainerName {
		t.Errorf("Name = %q, want %q", container.Name, backupContainerName)
	}
	wantEnv := map[string]string{
		"REMOTE_STORAGE": "s3",
		"S3_ENDPOINT":    "https://s3.amazonaws.com",
		"S3_BUCKET":      "my-bucket",
		"S3_REGION":      "us-east-1",
	}
	for _, e := range container.Env {
		if want, ok := wantEnv[e.Name]; ok && e.Value != want {
			t.Errorf("env %s = %q, want %q", e.Name, e.Value, want)
		}
	}
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != backupRESTPort {
		t.Errorf("unexpected ports: %+v", container.Ports)
	}
}

func TestBuildBackupContainer_NonS3Storage(t *testing.T) {
	storage := &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "not-s3", Namespace: "default"},
		Spec:       backupv1alpha1.BackupStorageSpec{Type: backupv1alpha1.BackupStorageTypeS3},
	}
	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "itest", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled: true,
				Storages: []corev1alpha1.InstanceBackupStorage{
					{StorageRef: apicommon.ObjectRef{Name: "not-s3"}},
				},
			},
		},
	}
	c := newTestContext(t, in, storage)

	if _, err := buildBackupContainer(c); err == nil {
		t.Error("expected an error when BackupStorage.Spec.S3 is nil")
	}
}

func TestChiHasBackupContainer(t *testing.T) {
	withSidecar := &chiv1.ClickHouseInstallation{
		Spec: chiv1.ChiSpec{
			Templates: &chiv1.Templates{
				PodTemplates: []chiv1.PodTemplate{
					{Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "clickhouse"},
						{Name: backupContainerName},
					}}},
				},
			},
		},
	}
	withoutSidecar := &chiv1.ClickHouseInstallation{
		Spec: chiv1.ChiSpec{
			Templates: &chiv1.Templates{
				PodTemplates: []chiv1.PodTemplate{
					{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "clickhouse"}}}},
				},
			},
		},
	}
	noTemplates := &chiv1.ClickHouseInstallation{}

	if !chiHasBackupContainer(withSidecar) {
		t.Error("expected true when the sidecar container is present")
	}
	if chiHasBackupContainer(withoutSidecar) {
		t.Error("expected false when the sidecar container is absent")
	}
	if chiHasBackupContainer(noTemplates) {
		t.Error("expected false when Templates is nil")
	}
}

func TestChiReplicaZeroHost(t *testing.T) {
	in := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "itest-ch", Namespace: "ns1"}}
	c := newTestContext(t, in)
	got := chiReplicaZeroHost(c)
	want := "chi-itest-ch-clickhouse-0-0.ns1.svc"
	if got != want {
		t.Errorf("chiReplicaZeroHost() = %q, want %q", got, want)
	}
}

func TestMapBackupState(t *testing.T) {
	tests := []struct {
		status string
		want   backupv1alpha1.BackupState
	}{
		{"success", backupv1alpha1.BackupStateSucceeded},
		{"error", backupv1alpha1.BackupStateFailed},
		{"in progress", backupv1alpha1.BackupStateRunning},
		{"", backupv1alpha1.BackupStateRunning},
	}
	for _, tt := range tests {
		if got := mapBackupState(tt.status); got != tt.want {
			t.Errorf("mapBackupState(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestMapRestoreState(t *testing.T) {
	tests := []struct {
		status string
		want   backupv1alpha1.RestoreState
	}{
		{"success", backupv1alpha1.RestoreStateSucceeded},
		{"error", backupv1alpha1.RestoreStateFailed},
		{"in progress", backupv1alpha1.RestoreStateRunning},
	}
	for _, tt := range tests {
		if got := mapRestoreState(tt.status); got != tt.want {
			t.Errorf("mapRestoreState(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestParseBackupTime(t *testing.T) {
	if _, ok := parseBackupTime(""); ok {
		t.Error("expected ok=false for empty string")
	}
	if _, ok := parseBackupTime("not-a-time"); ok {
		t.Error("expected ok=false for invalid time")
	}
	want := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	got, ok := parseBackupTime(want.Format(time.RFC3339))
	if !ok {
		t.Fatal("expected ok=true for a valid RFC3339 time")
	}
	if !got.Time.Equal(want) {
		t.Errorf("parseBackupTime() = %v, want %v", got.Time, want)
	}
}

func TestBackupExecutionStatus(t *testing.T) {
	entry := backupStatusEntry{Status: "success", Finish: "2026-07-24T12:00:00Z"}
	got := backupExecutionStatus(entry)
	if got.State != backupv1alpha1.BackupStateSucceeded {
		t.Errorf("State = %q, want %q", got.State, backupv1alpha1.BackupStateSucceeded)
	}
	if got.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}
}

func TestFindCommand(t *testing.T) {
	entries := []backupStatusEntry{
		{Command: "create_remote my-backup", Status: "success"},
		{Command: "restore_remote other-backup", Status: "in progress"},
	}
	if e := findCommand(entries, "create_remote", "my-backup"); e == nil {
		t.Error("expected to find the create_remote entry")
	}
	if e := findCommand(entries, "restore_remote", "my-backup"); e != nil {
		t.Error("expected no match for a mismatched backup name")
	}
	if e := findCommand(entries, "restore_remote", "other-backup"); e == nil {
		t.Error("expected to find the restore_remote entry")
	}
}

// =============================================================================
// backupClient (REST client) tests
// =============================================================================

func testClientAndServer(t *testing.T, handler http.HandlerFunc) *backupClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return newBackupClientWithPort(host, port)
}

func TestBackupClient_CreateRemote(t *testing.T) {
	var gotPath string
	c := testClientAndServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})

	if err := c.createRemote(context.Background(), "my backup"); err != nil {
		t.Fatalf("createRemote: %v", err)
	}
	if want := "/backup/create_remote?name=my+backup"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestBackupClient_RestoreRemote(t *testing.T) {
	var gotPath string
	c := testClientAndServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	})

	if err := c.restoreRemote(context.Background(), "my-backup"); err != nil {
		t.Fatalf("restoreRemote: %v", err)
	}
	if want := "/backup/restore_remote/my-backup"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestBackupClient_DeleteRemote(t *testing.T) {
	var gotPath string
	c := testClientAndServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	})

	if err := c.deleteRemote(context.Background(), "my-backup"); err != nil {
		t.Fatalf("deleteRemote: %v", err)
	}
	if want := "/backup/delete/remote/my-backup"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestBackupClient_ErrorResponse(t *testing.T) {
	c := testClientAndServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})

	err := c.createRemote(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected an error containing the response body, got %v", err)
	}
}

func TestBackupClient_Status(t *testing.T) {
	c := testClientAndServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backup/status" {
			t.Errorf("path = %q, want /backup/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"command":"create_remote x","status":"success"}]`))
	})

	entries, err := c.status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != "success" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}
