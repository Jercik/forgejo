// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	"forgejo.org/models/unit"
	"forgejo.org/modules/container"
)

// ErrRerunNoFailedJobs signals that the run cannot be rerun because none of its jobs failed or was cancelled.
var ErrRerunNoFailedJobs = errors.New("run has no failed or cancelled jobs")

// collectFailedRerunJobs determines which jobs of a run have to be rerun when the failed jobs of that run are rerun. It
// returns those jobs in id order, together with the set of workflow-level job ids they cover.
//
// Membership is tracked per ActionRunJob row because matrix expansions produce multiple rows that share one workflow
// job id, while "needs" is matched against workflow job ids. The expansion is a fixpoint, not a single pass: jobs come
// back in insertion order, which is the order the jobs are declared in the workflow, and a job may be declared before
// the job it depends on.
func collectFailedRerunJobs(allJobs []*actions_model.ActionRunJob) ([]*actions_model.ActionRunJob, container.Set[string]) {
	memberIDs := make(container.Set[int64])
	rerunJobIDs := make(container.Set[string])
	var members []*actions_model.ActionRunJob

	addMember := func(job *actions_model.ActionRunJob) {
		memberIDs.Add(job.ID)
		rerunJobIDs.Add(job.JobID)
		members = append(members, job)
	}

	// The seeds are all jobs that failed or were cancelled. Successful or skipped jobs are never seeds, not even a
	// successful sibling of a failed matrix job.
	for _, job := range allJobs {
		if job.Status == actions_model.StatusFailure || job.Status == actions_model.StatusCancelled {
			addMember(job)
		}
	}

	if len(members) == 0 {
		return nil, rerunJobIDs
	}

	// Add the dependents of the seeds, and their dependents, until nothing changes. Dependents are added regardless of
	// the status they have now, and all rows of a dependent matrix job are added, not just the first one.
	for {
		added := false
		for _, job := range allJobs {
			if memberIDs.Contains(job.ID) {
				continue
			}
			if slices.ContainsFunc(job.Needs, rerunJobIDs.Contains) {
				addMember(job)
				added = true
			}
		}
		if !added {
			break
		}
	}

	slices.SortFunc(members, func(a, b *actions_model.ActionRunJob) int {
		return cmp.Compare(a.ID, b.ID)
	})

	return members, rerunJobIDs
}

// RerunFailedJobs reruns all failed or cancelled jobs of the given run and all their dependent jobs, and returns the
// jobs that were rerun. For it to succeed, the workflow must be valid, the previous run must have completed, and the
// run must have at least one failed or cancelled job.
func RerunFailedJobs(ctx context.Context, run *actions_model.ActionRun) ([]*actions_model.ActionRunJob, error) {
	if !run.IsValid() {
		return nil, ErrRerunWorkflowInvalid
	}
	if !run.Status.IsDone() {
		return nil, ErrRerunWorkflowStillRunning
	}

	if err := run.LoadRepo(ctx); err != nil {
		return nil, fmt.Errorf("cannot load repo of run %d: %w", run.ID, err)
	}

	actionsConfig := run.Repo.MustGetUnit(ctx, unit.TypeActions).ActionsConfig()
	if actionsConfig.IsWorkflowDisabled(run.WorkflowID) {
		return nil, ErrRerunWorkflowDisabled
	}

	var rerunJobs []*actions_model.ActionRunJob
	if err := db.WithTx(ctx, func(ctx context.Context) error {
		rerunJobs = nil

		currentRun, err := actions_model.GetRunByID(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("cannot load run %d: %w", run.ID, err)
		}
		if currentRun.Status != actions_model.StatusUnknown && !currentRun.Status.IsDone() {
			return fmt.Errorf("cannot prepare next attempt because run %d is active: %s", currentRun.ID, currentRun.Status.String())
		}

		// The duration has to be captured before any job is touched, because the run row is updated as a side effect of
		// updating its jobs.
		previousDuration := currentRun.Duration()

		jobs, err := actions_model.GetRunJobsByRunID(ctx, currentRun.ID)
		if err != nil {
			return fmt.Errorf("could not load jobs of run %d: %w", currentRun.ID, err)
		}

		members, rerunJobIDs := collectFailedRerunJobs(jobs)
		if len(members) == 0 {
			return ErrRerunNoFailedJobs
		}

		// Wipe all artifacts before a rerun to prevent stale artifacts from polluting the artifacts collected during
		// the rerun. Because artifacts are bound to a run and not to a job, artifacts created by jobs that are not
		// rerun will be lost, too.
		if err := actions_model.SetArtifactsOfRunDeleted(ctx, currentRun.ID); err != nil {
			return fmt.Errorf("cannot remove artifacts of previous run of run %d: %w", currentRun.ID, err)
		}

		// Phase 1: cancel every member that is still running. A done run does not imply that all of its jobs are done:
		// a job with `if: always()` that depends on a failed job keeps running while the run already aggregates as
		// failed. Cancelling all of them before any job is flipped keeps the aggregated run status done for the whole
		// phase, so no "run is now done" notification can fire.
		for i, member := range members {
			if member.Status.IsDone() {
				continue
			}

			if err := cancelSingleJob(ctx, member, actions_model.StatusCancelled); err != nil {
				return fmt.Errorf("cannot cancel job %d with status %s: %w", member.ID, member.Status, err)
			}

			// Refresh the job after cancellation.
			refreshedMember, err := actions_model.GetRunJobByID(ctx, member.ID)
			if err != nil {
				return fmt.Errorf("cannot refresh cancelled job %d: %w", member.ID, err)
			}
			members[i] = refreshedMember
		}

		// Phase 2: flip every member to its new attempt. Every failed or cancelled job of the run is a member, so the
		// aggregated run status leaves the done state exactly once, at the last of these updates, and never returns to
		// it, because jobs only become waiting or blocked here.
		for _, member := range members {
			canBeRerun, err := member.CanBeRerun(ctx)
			if err != nil {
				return fmt.Errorf("cannot determine whether job %d can be rerun: %w", member.ID, err)
			}

			// This should never happen because the run was validated and every running member was cancelled above.
			if !canBeRerun {
				return fmt.Errorf("cannot rerun job %d", member.ID)
			}

			// A job whose needs are all satisfied by jobs that are not rerun has to start waiting instead of blocked:
			// blocked jobs are only released when a runner reports a task of this run as done, so a blocked job without
			// a rerunning dependency would deadlock.
			initialStatus := actions_model.StatusWaiting
			if slices.ContainsFunc(member.Needs, rerunJobIDs.Contains) {
				initialStatus = actions_model.StatusBlocked
			}

			if err := rerunSingleJob(ctx, member, initialStatus); err != nil {
				return fmt.Errorf("could not rerun job %d of run %d: %w", member.ID, currentRun.ID, err)
			}

			rerunJobs = append(rerunJobs, member)
		}

		// Phase 3: reset the run row last, and never write its status column. Updating the jobs above already persisted
		// the correct aggregated status, which is not necessarily waiting: a job of this run that is not rerun may
		// still be running.
		updatedRun, err := actions_model.GetRunByID(ctx, currentRun.ID)
		if err != nil {
			return fmt.Errorf("cannot reload run %d: %w", currentRun.ID, err)
		}

		updatedRun.Started = 0
		updatedRun.Stopped = 0
		updatedRun.PreviousDuration = previousDuration
		updatedRun.Priority = actions_model.DefaultRunPriority
		updatedRun.Prioritize = false

		// The columns have to be specified here to work around a xorm quirk: It won't update columns that are set to
		// their zero value without AllCols().
		if err := UpdateRun(ctx, updatedRun, "started", "stopped", "previous_duration", "priority", "prioritize"); err != nil {
			return fmt.Errorf("cannot update run %d: %w", updatedRun.ID, err)
		}

		if err := recalculateRunPriorities(ctx, updatedRun.RepoID); err != nil {
			return fmt.Errorf("could not recalculate workflow run priorities of repository %d: %w", updatedRun.RepoID, err)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return rerunJobs, nil
}
