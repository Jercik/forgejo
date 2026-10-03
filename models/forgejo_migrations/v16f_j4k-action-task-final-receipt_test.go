// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo_migrations

import (
	"database/sql"
	"fmt"
	"testing"

	migration_tests "forgejo.org/models/gitea_migrations/test"

	"github.com/stretchr/testify/require"
)

func TestAddTaskFinalReceiptPreservesLegacyNULL(t *testing.T) {
	type ActionTask struct {
		ID     int64 `xorm:"pk"`
		Status int64
	}
	x, cleanup := migration_tests.PrepareTestEnv(t, 0, new(ActionTask))
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Insert(&ActionTask{ID: 1, Status: 3})
	require.NoError(t, err)
	require.NoError(t, addTaskFinalReceipt(x))

	var legacy struct {
		Status                    int64
		RunnerFinalReportReceived sql.NullBool
	}
	found, err := x.Table("action_task").Where("id = ?", 1).Get(&legacy)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(3), legacy.Status)
	require.Equal(t, sql.NullBool{}, legacy.RunnerFinalReportReceived)

	_, err = x.Exec("INSERT INTO action_task (id, status, runner_final_report_received) VALUES (?, ?, ?)", 2, 1, false)
	require.NoError(t, err)
	_, err = x.Exec("INSERT INTO action_task (id, status, runner_final_report_received) VALUES (?, ?, ?)", 3, 1, true)
	require.NoError(t, err)
	var pending struct{ RunnerFinalReportReceived sql.NullBool }
	found, err = x.Table("action_task").Where("id = ?", 2).Get(&pending)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, sql.NullBool{Valid: true, Bool: false}, pending.RunnerFinalReportReceived)
	var accepted struct{ RunnerFinalReportReceived sql.NullBool }
	found, err = x.Table("action_task").Where("id = ?", 3).Get(&accepted)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, sql.NullBool{Valid: true, Bool: true}, accepted.RunnerFinalReportReceived)
	require.NoError(t, addTaskFinalReceipt(x))
}

func TestTaskFinalReceiptRegisteredUpgrade(t *testing.T) {
	type ActionTask struct {
		ID     int64 `xorm:"pk"`
		Status int64
	}
	x, cleanup := migration_tests.PrepareTestEnv(t, 0, new(ActionTask), new(ForgejoMigration))
	defer cleanup()
	if x == nil || t.Failed() {
		return
	}
	_, err := x.Insert(&ActionTask{ID: 1, Status: 3})
	require.NoError(t, err)
	require.NoError(t, receiptPriorMigrationRecords(x))
	require.NoError(t, Migrate(x, false))
	var receipt struct{ RunnerFinalReportReceived sql.NullBool }
	found, err := x.Table("action_task").Where("id = ?", 1).Get(&receipt)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, sql.NullBool{}, receipt.RunnerFinalReportReceived)
	var applied ForgejoMigration
	found, err = x.ID("v16f_j4k-action-task-final-receipt").Get(&applied)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "v16f_j4k-action-task-final-receipt", applied.ID)
	require.NoError(t, Migrate(x, false))
}

func receiptPriorMigrationRecords(x interface{ Insert(...any) (int64, error) }) error {
	resolveMigrations()
	found := false
	for _, migration := range orderedMigrations {
		if migration.id == "v16f_j4k-action-task-final-receipt" {
			found = true
			continue
		}
		if _, err := x.Insert(&ForgejoMigration{ID: migration.id}); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("receipt migration is not registered")
	}
	return nil
}
