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
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newRerunNotifier returns a registered notifier that tolerates job-level notifications but fails the test when the run
// is announced as completed, because a rerun of failed jobs must not report the run as done in between.
func newRerunNotifier(t *testing.T) *notify_service.MockNotifier {
	t.Helper()
	notifier := notify_service.NewMockNotifier(t)
	notifier.On("Run").Return().Maybe()
	notifier.On("NewWorkflowRunAttempt", mock.Anything, mock.Anything).Return().Maybe()
	notifier.On("WorkflowRunStatusChanged", mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	notifier.On("NewWorkflowJobAttempt", mock.Anything, mock.Anything).Return().Maybe()
	notifier.On("WorkflowJobStatusChanged", mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	notifier.On("WorkflowJobCompleted", mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	notify_service.RegisterNotifier(notifier)
	t.Cleanup(func() { notify_service.UnregisterNotifier(notifier) })
	return notifier
}

func TestRerunFailedJobs(t *testing.T) {
	t.Run("Reruns all failed jobs without run notifications", func(t *testing.T) {
		defer unittest.OverrideFixtures("services/actions/TestRerunFailedJobs")()
		require.NoError(t, unittest.PrepareTestDatabase())

		notifier := newRerunNotifier(t)

		var recalculateRepoTarget *int64
		defer test.MockVariableValue(&recalculateRunPriorities, func(_ context.Context, repoID int64) error {
			recalculateRepoTarget = &repoID
			return nil
		})()

		run := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455720})

		rerunJobs, err := RerunFailedJobs(t.Context(), run)
		require.NoError(t, err)

		notifier.AssertNotCalled(t, "WorkflowRunCompleted", mock.Anything, mock.Anything, mock.Anything)
		notifier.AssertNumberOfCalls(t, "NewWorkflowRunAttempt", 1)
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

	t.Run("Cancels a running dependent without run notifications", func(t *testing.T) {
		defer unittest.OverrideFixtures("services/actions/TestRerunFailedJobs")()
		require.NoError(t, unittest.PrepareTestDatabase())

		notifier := newRerunNotifier(t)

		defer test.MockVariableValue(&recalculateRunPriorities, func(_ context.Context, repoID int64) error {
			return nil
		})()

		run := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455730})
		runningJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683961})
		require.Equal(t, actions_model.StatusRunning, runningJob.Status)
		require.Equal(t, int64(99001), runningJob.TaskID)

		rerunJobs, err := RerunFailedJobs(t.Context(), run)
		require.NoError(t, err)

		notifier.AssertNotCalled(t, "WorkflowRunCompleted", mock.Anything, mock.Anything, mock.Anything)

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

		notifier := newRerunNotifier(t)

		run := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455740})

		rerunJobs, err := RerunFailedJobs(t.Context(), run)
		require.ErrorIs(t, err, ErrRerunNoFailedJobs)
		assert.Empty(t, rerunJobs)
		notifier.AssertNotCalled(t, "WorkflowRunCompleted", mock.Anything, mock.Anything, mock.Anything)

		run = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: 455740})
		assert.Equal(t, actions_model.StatusSuccess, run.Status)
		assert.Equal(t, timeutil.TimeStamp(1776279254), run.Started)
		assert.Equal(t, timeutil.TimeStamp(1776279265), run.Stopped)

		job := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: 683970})
		assert.Equal(t, actions_model.StatusSuccess, job.Status)
		assert.Equal(t, int64(1), job.Attempt)
	})
}

func TestCollectFailedRerunJobs(t *testing.T) {
	job := func(id int64, ns, jobID string, status actions_model.Status, needs ...string) *actions_model.ActionRunJob {
		localNeeds := make([]actions_model.LocalJobIdentifier, len(needs))
		for i, need := range needs {
			localNeeds[i] = actions_model.LocalJobIdentifier(need)
		}
		return &actions_model.ActionRunJob{
			ID:           id,
			JobNamespace: actions_model.JobNamespace(ns),
			JobID:        actions_model.JobIdentifier(jobID),
			Status:       status,
			Needs:        localNeeds,
		}
	}

	testCases := []struct {
		name        string
		jobs        []*actions_model.ActionRunJob
		expectedIDs []int64
	}{
		{
			name: "No failed jobs",
			jobs: []*actions_model.ActionRunJob{
				job(1, "", "build", actions_model.StatusSuccess),
				job(2, "", "test", actions_model.StatusSkipped, "build"),
			},
			expectedIDs: nil,
		},
		{
			name: "Successful job without failed dependency stays out",
			jobs: []*actions_model.ActionRunJob{
				job(1, "", "lint", actions_model.StatusSuccess),
				job(2, "", "build", actions_model.StatusFailure),
				job(3, "", "test", actions_model.StatusSkipped, "build"),
			},
			expectedIDs: []int64{2, 3},
		},
		{
			name: "Dependent declared before the job it needs",
			jobs: []*actions_model.ActionRunJob{
				job(1, "", "deploy", actions_model.StatusSkipped, "test"),
				job(2, "", "test", actions_model.StatusSkipped, "build"),
				job(3, "", "build", actions_model.StatusFailure),
			},
			expectedIDs: []int64{1, 2, 3},
		},
		{
			name: "Successful sibling rows of a failed matrix job stay out, all dependent rows join",
			jobs: []*actions_model.ActionRunJob{
				job(1, "", "build", actions_model.StatusSuccess),
				job(2, "", "build", actions_model.StatusFailure),
				job(3, "", "build", actions_model.StatusSuccess),
				job(4, "", "test", actions_model.StatusSkipped, "build"),
				job(5, "", "test", actions_model.StatusSkipped, "build"),
			},
			expectedIDs: []int64{2, 4, 5},
		},
		{
			name: "Matrix rows of a dependent join as a whole",
			jobs: []*actions_model.ActionRunJob{
				job(1, "", "build", actions_model.StatusFailure),
				job(2, "", "test", actions_model.StatusSuccess, "build"),
				job(3, "", "test", actions_model.StatusSuccess, "build"),
				job(4, "", "other", actions_model.StatusSuccess),
			},
			expectedIDs: []int64{1, 2, 3},
		},
		{
			name: "Namespaced needs match within the same namespace only",
			jobs: []*actions_model.ActionRunJob{
				job(1, "ns1", "build", actions_model.StatusFailure),
				job(2, "ns1", "test", actions_model.StatusSkipped, "build"),
				job(3, "ns2", "build", actions_model.StatusSuccess),
				job(4, "ns2", "test", actions_model.StatusSuccess, "build"),
				job(5, "", "build", actions_model.StatusSuccess),
				job(6, "", "test", actions_model.StatusSuccess, "build"),
			},
			expectedIDs: []int64{1, 2},
		},
		{
			name: "Fully qualified needs cross namespaces",
			jobs: []*actions_model.ActionRunJob{
				job(1, "ns1", "build", actions_model.StatusFailure),
				job(2, "ns2", "test", actions_model.StatusSuccess, "__namespace.ns1.build"),
				job(3, "ns2", "lint", actions_model.StatusSuccess),
			},
			expectedIDs: []int64{1, 2},
		},
		{
			name: "Cancelled job is a seed and members are returned in id order",
			jobs: []*actions_model.ActionRunJob{
				job(9, "", "c", actions_model.StatusSuccess, "b"),
				job(7, "", "a", actions_model.StatusSuccess),
				job(8, "", "b", actions_model.StatusCancelled, "a"),
			},
			expectedIDs: []int64{8, 9},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			members, rerunJobIDs := collectFailedRerunJobs(tc.jobs)

			ids := make([]int64, 0, len(members))
			for _, member := range members {
				ids = append(ids, member.ID)
				assert.True(t, rerunJobIDs.Contains(member.NamespacedJobID()))
			}
			if len(tc.expectedIDs) == 0 {
				assert.Empty(t, ids)
			} else {
				assert.Equal(t, tc.expectedIDs, ids)
			}
		})
	}
}
