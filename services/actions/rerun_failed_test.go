// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"testing"
	"time"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/unittest"
	"forgejo.org/modules/test"
	"forgejo.org/modules/timeutil"
	notify_service "forgejo.org/services/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRerunFailedJobs(t *testing.T) {
	t.Run("Reruns all failed jobs without notification", func(t *testing.T) {
		defer unittest.OverrideFixtures("services/actions/TestRerunFailedJobs")()
		require.NoError(t, unittest.PrepareTestDatabase())

		notifier := &mockNotifier{}
		notify_service.RegisterNotifier(notifier)
		defer notify_service.UnregisterNotifier(notifier)

		var recalculateRepoTarget *int64
		defer test.MockVariableValue(&recalculateRunPriorities, func(_ context.Context, repoID int64) error {
			recalculateRepoTarget = &repoID
			return nil
		})()

		run := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455720})

		rerunJobs, err := RerunFailedJobs(t.Context(), run)
		require.NoError(t, err)

		assert.Empty(t, notifier.calls)
		assert.Equal(t, &run.RepoID, recalculateRepoTarget)

		assert.Len(t, rerunJobs, 2)
		assert.Equal(t, int64(683951), rerunJobs[0].ID)
		assert.Equal(t, int64(683952), rerunJobs[1].ID)

		run = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455720})
		assert.Equal(t, actions_model.StatusWaiting, run.Status)
		assert.Equal(t, timeutil.TimeStamp(0), run.Started)
		assert.Equal(t, timeutil.TimeStamp(0), run.Stopped)
		assert.Equal(t, 11*time.Second, run.PreviousDuration)
		assert.Equal(t, actions_model.DefaultRunPriority, run.Priority)
		assert.False(t, run.Prioritize)

		untouchedJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683950})
		assert.Equal(t, actions_model.StatusSuccess, untouchedJob.Status)
		assert.Equal(t, int64(1), untouchedJob.Attempt)

		for _, jobID := range []int64{683951, 683952} {
			job := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: jobID})
			assert.Equal(t, actions_model.StatusWaiting, job.Status)
			assert.Equal(t, int64(2), job.Attempt)
			assert.Equal(t, timeutil.TimeStamp(0), job.Started)
			assert.Equal(t, timeutil.TimeStamp(0), job.Stopped)
		}
	})

	t.Run("Cancels a running dependent without notification", func(t *testing.T) {
		defer unittest.OverrideFixtures("services/actions/TestRerunFailedJobs")()
		require.NoError(t, unittest.PrepareTestDatabase())

		notifier := &mockNotifier{}
		notify_service.RegisterNotifier(notifier)
		defer notify_service.UnregisterNotifier(notifier)

		defer test.MockVariableValue(&recalculateRunPriorities, func(_ context.Context, repoID int64) error {
			return nil
		})()

		run := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455730})
		runningJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683961})
		require.Equal(t, actions_model.StatusRunning, runningJob.Status)
		require.Equal(t, int64(99001), runningJob.TaskID)

		rerunJobs, err := RerunFailedJobs(t.Context(), run)
		require.NoError(t, err)

		assert.Empty(t, notifier.calls)

		assert.Len(t, rerunJobs, 2)
		assert.Equal(t, int64(683960), rerunJobs[0].ID)
		assert.Equal(t, int64(683961), rerunJobs[1].ID)

		run = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455730})
		assert.Equal(t, actions_model.StatusWaiting, run.Status)
		assert.Equal(t, timeutil.TimeStamp(0), run.Started)
		assert.Equal(t, timeutil.TimeStamp(0), run.Stopped)

		failedJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683960})
		assert.Equal(t, actions_model.StatusWaiting, failedJob.Status)
		assert.Equal(t, int64(2), failedJob.Attempt)

		dependentJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683961})
		assert.Equal(t, actions_model.StatusBlocked, dependentJob.Status)
		assert.Equal(t, int64(2), dependentJob.Attempt)
		assert.Equal(t, timeutil.TimeStamp(0), dependentJob.Started)
		assert.Equal(t, timeutil.TimeStamp(0), dependentJob.Stopped)

		task := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 99001})
		assert.Equal(t, actions_model.StatusCancelled, task.Status)
	})

	t.Run("Error if run has no failed jobs", func(t *testing.T) {
		defer unittest.OverrideFixtures("services/actions/TestRerunFailedJobs")()
		require.NoError(t, unittest.PrepareTestDatabase())

		notifier := &mockNotifier{}
		notify_service.RegisterNotifier(notifier)
		defer notify_service.UnregisterNotifier(notifier)

		run := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455740})

		rerunJobs, err := RerunFailedJobs(t.Context(), run)
		require.ErrorIs(t, err, ErrRerunNoFailedJobs)
		assert.Empty(t, rerunJobs)
		assert.Empty(t, notifier.calls)

		run = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455740})
		assert.Equal(t, actions_model.StatusSuccess, run.Status)
		assert.Equal(t, timeutil.TimeStamp(1776279254), run.Started)
		assert.Equal(t, timeutil.TimeStamp(1776279265), run.Stopped)

		job := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683970})
		assert.Equal(t, actions_model.StatusSuccess, job.Status)
		assert.Equal(t, int64(1), job.Attempt)
	})
}
