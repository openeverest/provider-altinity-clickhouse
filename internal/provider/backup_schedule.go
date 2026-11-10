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
	"strings"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// scheduleCRName returns the ClickHouseBackupSchedule resource name for an instance and schedule.
func scheduleCRName(instance, schedule string) string {
	return instance + "-" + schedule
}

// syncBackupSchedules reconciles ClickHouseBackupSchedule objects for the instance:
// it applies one CHBS per schedule when backup is enabled and deletes any CHBS owned
// by this instance that are no longer desired.
func syncBackupSchedules(c *controller.Context) error {
	desired := make(map[string]bool)

	if backupEnabled(c) {
		for _, storage := range c.Instance().Spec.Backup.Storages {
			for _, schedule := range storage.Schedules {
				crName := scheduleCRName(c.Name(), schedule.Name)
				desired[crName] = true

				params, err := decodeBackupParameters(schedule.Parameters)
				if err != nil {
					return fmt.Errorf("decode backup parameters for schedule %s: %w", schedule.Name, err)
				}

				spec := chiv1.ClickHouseBackupScheduleSpec{
					ClickHouseInstallation: c.Name(),
					Schedule:               schedule.Cron,
					Suspend:                chtypes.NewStringBool(!schedule.Enabled),
				}
				if params.ReplicaSelection != "" {
					spec.ReplicaSelection = chiv1.ReplicaSelection(params.ReplicaSelection)
				}
				if schedule.Retention != nil && schedule.Retention.Type == corev1alpha1.BackupScheduleRetentionTypeCount {
					spec.Retention = chiv1.BackupRetention{
						KeepLast: schedule.Retention.Count,
					}
				}

				chbs := &chiv1.ClickHouseBackupSchedule{
					TypeMeta: metav1.TypeMeta{
						APIVersion: chiv1.SchemeGroupVersion.String(),
						Kind:       "ClickHouseBackupSchedule",
					},
					ObjectMeta: c.ObjectMeta(crName),
					Spec:       spec,
				}
				if err := c.Apply(chbs); err != nil {
					return fmt.Errorf("apply ClickHouseBackupSchedule %s: %w", crName, err)
				}
			}
		}
	}

	list := &chiv1.ClickHouseBackupScheduleList{}
	if err := c.List(list); err != nil {
		return fmt.Errorf("list ClickHouseBackupSchedules: %w", err)
	}

	for i := range list.Items {
		item := &list.Items[i]
		owner := metav1.GetControllerOf(item)
		if owner == nil || owner.UID != c.Instance().UID {
			continue
		}
		if !desired[item.Name] {
			// Deleting a CHBS removes its CronJob but leaves its backups in storage.
			if err := c.Delete(item); err != nil {
				return fmt.Errorf("delete ClickHouseBackupSchedule %s: %w", item.Name, err)
			}
		}
	}

	return nil
}

// validateBackupSchedules rejects schedule configurations the Altinity operator cannot honour.
// It returns nil when backup is nil or disabled.
func validateBackupSchedules(backup *corev1alpha1.InstanceBackupSpec, instance string) error {
	if backup == nil || !backup.Enabled {
		return nil
	}

	for _, storage := range backup.Storages {
		for _, schedule := range storage.Schedules {
			crName := scheduleCRName(instance, schedule.Name)
			if errs := validation.IsDNS1123Label(crName); len(errs) > 0 {
				return fmt.Errorf("schedule %q: resource name %q is not a valid DNS-1123 label: %s",
					schedule.Name, crName, strings.Join(errs, "; "))
			}

			cron := strings.TrimSpace(schedule.Cron)
			if cron == "" || cron == "@" || (!strings.HasPrefix(cron, "@") && len(strings.Fields(cron)) != 5) {
				return fmt.Errorf("schedule %q: cron expression %q must have exactly 5 space-separated fields or start with '@'",
					schedule.Name, schedule.Cron)
			}

			if schedule.Retention != nil {
				if schedule.Retention.Type == corev1alpha1.BackupScheduleRetentionTypeTime || schedule.Retention.Type == "time" {
					return fmt.Errorf("schedule %q: the clickhouse-backup operator only supports count retention (keepLast); use type count",
						schedule.Name)
				}
			}

			params, err := decodeBackupParameters(schedule.Parameters)
			if err != nil {
				return fmt.Errorf("schedule %q: %w", schedule.Name, err)
			}
			if params.ReplicaSelection != "" &&
				params.ReplicaSelection != string(chiv1.ReplicaSelectionFirstPerShard) &&
				params.ReplicaSelection != string(chiv1.ReplicaSelectionAllReplicas) {
				return fmt.Errorf("schedule %q: invalid replicaSelection %q: allowed values are %q and %q",
					schedule.Name, params.ReplicaSelection, chiv1.ReplicaSelectionFirstPerShard, chiv1.ReplicaSelectionAllReplicas)
			}
		}
	}

	return nil
}
