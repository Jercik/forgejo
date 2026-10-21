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
// returns those jobs in id order, together with the set of namespaced workflow-level job ids they cover.
//
// Membership is tracked per ActionRunJob row because matrix expansions produce multiple rows that share one workflow
// job id, while "needs" is matched against namespaced workflow job ids. The expansion is a fixpoint, not a single pass: jobs come
// back in insertion order, which is the order the jobs are declared in the workflow, and a job may be declared before
// the job it depends on.
func collectFailedRerunJobs(allJobs []*actions_model.ActionRunJob) ([]*actions_model.ActionRunJob, container.Set[actions_model.NamespacedJobIdentifier]) {
	memberIDs := make(container.Set[int64])
	rerunJobIDs := make(container.Set[actions_model.NamespacedJobIdentifier])
	var members []*actions_model.ActionRunJob

	addMember := func(job *actions_model.ActionRunJob) {
		memberIDs.Add(job.ID)
		rerunJobIDs.Add(job.NamespacedJobID())
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
			if slices.ContainsFunc(job.NamespacedNeeds(), rerunJobIDs.Contains) {
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

		// Start the next attempt of the run before touching its jobs: this captures the previous duration, resets the
		// timestamps and priority, and notifies observers of the new attempt.
		if err := InitiateNextRunAttempt(ctx, currentRun); err != nil {
			return fmt.Errorf("could not initiate next attempt of run %d: %w", currentRun.ID, err)
		}

		if err := recalculateRunPriorities(ctx, currentRun.RepoID); err != nil {
			return fmt.Errorf("could not recalculate workflow run priorities of repository %d: %w", currentRun.RepoID, err)
		}

		// Phase 1: cancel every member that is not done yet. The run is done, so none should be running; this is a
		// defensive guard against stale or racing job rows, mirroring the cancel loop of RerunJob in rerun.go.
		// Cancelling them first lets every member be flipped to a new attempt.
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

		// Phase 2: flip every member to its new attempt. Jobs only become waiting or blocked here.
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
			if slices.ContainsFunc(member.NamespacedNeeds(), rerunJobIDs.Contains) {
				initialStatus = actions_model.StatusBlocked
			}

			if err := InitiateNextJobAttempt(ctx, member, initialStatus); err != nil {
				return fmt.Errorf("could not rerun job %d of run %d: %w", member.ID, currentRun.ID, err)
			}

			rerunJobs = append(rerunJobs, member)
		}

		// Phase 3: refresh the run status from its jobs. A job of this run that is not rerun may still be running, so
		// the status is aggregated rather than assumed to be waiting.
		if err := RefreshAndPropagateRunStatus(ctx, currentRun.ID); err != nil {
			return fmt.Errorf("could not refresh and propagate the status of run %d: %w", currentRun.ID, err)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return rerunJobs, nil
}
