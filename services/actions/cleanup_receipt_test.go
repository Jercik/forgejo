// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package actions

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	"forgejo.org/models/unittest"
	actions_module "forgejo.org/modules/actions"
	"forgejo.org/modules/timeutil"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func receiptCleanupTask(t *testing.T, id int64, withLogs bool) *actions_model.ActionTask {
	t.Helper()
	task := &actions_model.ActionTask{ID: id, RepoID: 1, RunnerID: 1, Status: actions_model.StatusSuccess, Stopped: 1, TokenHash: fmt.Sprintf("cleanup-receipt-%d", id)}
	if withLogs {
		task.LogFilename = fmt.Sprintf("cleanup-receipt/%d.log", id)
		ns, err := actions_module.WriteLogs(t.Context(), task.LogFilename, 0, []*runnerv1.LogRow{{Time: timestamppb.Now(), Content: "retained pending log"}})
		require.NoError(t, err)
		task.LogIndexes = []int64{0}
		task.LogLength = 1
		task.LogSize = int64(ns[0])
	}
	unittest.AssertSuccessfulInsert(t, task)
	return task
}

func TestCleanupReceiptEnrollmentBeforeExpirationPreservesLogs(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	task := receiptCleanupTask(t, 990007, true)
	selected, err := actions_model.FindOldTasksToExpire(t.Context(), timeutil.TimeStamp(2), 100, 0)
	require.NoError(t, err)
	require.Contains(t, func() []int64 {
		ids := make([]int64, len(selected))
		for i, task := range selected {
			ids[i] = task.ID
		}
		return ids
	}(), task.ID)
	require.NoError(t, db.WithTx(t.Context(), func(ctx context.Context) error { return actions_model.EnrollTaskReceipt(ctx, task) }))
	expired, err := expireTaskLogs(t.Context(), task.ID, 2)
	require.NoError(t, err)
	require.False(t, expired)
	current := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: task.ID})
	require.Equal(t, sql.NullBool{Valid: true}, current.RunnerFinalReportReceived)
	require.False(t, current.LogExpired)
	require.Equal(t, task.LogIndexes, current.LogIndexes)
	rows, err := actions_module.ReadLogs(t.Context(), false, task.LogFilename, 0, -1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "retained pending log", rows[0].Content)
}

func TestCleanupReceiptExpirationBeforeEnrollment(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	task := receiptCleanupTask(t, 990008, true)
	expired, err := expireTaskLogs(t.Context(), task.ID, 2)
	require.NoError(t, err)
	require.True(t, expired)
	_, err = actions_module.OpenLogs(t.Context(), false, task.LogFilename)
	require.Error(t, err)
	current := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: task.ID})
	require.False(t, current.RunnerFinalReportReceived.Valid)
	require.True(t, current.LogExpired)
	require.Empty(t, current.LogIndexes)
	// Expiration completed before this legacy enrollment; it is not pending-log destruction.
	require.NoError(t, db.WithTx(t.Context(), func(ctx context.Context) error { return actions_model.EnrollTaskReceipt(ctx, task) }))
	current = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: task.ID})
	require.Equal(t, sql.NullBool{Valid: true}, current.RunnerFinalReportReceived)
	require.True(t, current.LogExpired)
}

func TestCleanupReceiptSkipsFullEnrolledBatchAndProgresses(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	_, err := db.GetEngine(t.Context()).Where("id > 0").Cols("stopped").Update(&actions_model.ActionTask{Stopped: timeutil.TimeStampNow()})
	require.NoError(t, err)
	var tasks []*actions_model.ActionTask
	for i := int64(0); i < deleteLogBatchSize; i++ {
		tasks = append(tasks, receiptCleanupTask(t, 990100+i, false))
	}
	next := receiptCleanupTask(t, 990200, false)
	calls := 0
	require.NoError(t, cleanupLogs(t.Context(), func(ctx context.Context, cutoff timeutil.TimeStamp, limit int, afterID int64) ([]*actions_model.ActionTask, error) {
		selected, err := actions_model.FindOldTasksToExpire(ctx, cutoff, limit, afterID)
		if err != nil {
			return nil, err
		}
		calls++
		if calls == 1 {
			require.Len(t, selected, deleteLogBatchSize)
			err = db.WithTx(ctx, func(ctx context.Context) error {
				for _, task := range tasks {
					if err := actions_model.EnrollTaskReceipt(ctx, task); err != nil {
						return err
					}
				}
				return nil
			})
		}
		return selected, err
	}))
	require.Equal(t, 2, calls, "full skipped batch must advance to the next batch once")
	for _, task := range tasks {
		current := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: task.ID})
		require.Equal(t, sql.NullBool{Valid: true}, current.RunnerFinalReportReceived)
		require.False(t, current.LogExpired)
	}
	current := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: next.ID})
	require.True(t, current.LogExpired)
}
