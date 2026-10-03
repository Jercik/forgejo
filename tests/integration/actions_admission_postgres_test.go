// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package integration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	"forgejo.org/modules/setting"
	repository_service "forgejo.org/services/repository"
	"forgejo.org/tests"

	"github.com/stretchr/testify/require"
)

func admissionPostgresEnv(t *testing.T) func() {
	t.Helper()
	if !setting.Database.Type.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock serialization")
	}
	return tests.PrepareTestEnv(t)
}

func admissionPostgresPID(ctx context.Context) (int64, error) {
	var row struct{ PID int64 }
	_, err := db.GetEngine(ctx).SQL("SELECT pg_backend_pid() AS pid").Get(&row)
	return row.PID, err
}

func admissionPostgresBlocked(t *testing.T, pid int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		var row struct{ Blocked bool }
		_, err := db.GetEngine(t.Context()).SQL("SELECT cardinality(pg_blocking_pids(?)) > 0 AS blocked", pid).Get(&row)
		return err == nil && row.Blocked
	}, 5*time.Second, 10*time.Millisecond, "competing transaction never waited for the repository row lock")
}

func admissionPostgresDelete(t *testing.T, operation func(context.Context) error) (<-chan int64, <-chan error) {
	t.Helper()
	pid, result := make(chan int64, 1), make(chan error, 1)
	go func() {
		result <- db.WithTx(t.Context(), func(ctx context.Context) error {
			backend, err := admissionPostgresPID(ctx)
			if err != nil {
				return err
			}
			pid <- backend
			return operation(ctx)
		})
	}()
	return pid, result
}

func TestAdmissionPostgresEnrollmentSerializesTaskDeletion(t *testing.T) {
	defer admissionPostgresEnv(t)()
	task := &actions_model.ActionTask{ID: 20001, RepoID: 1, RunnerID: 90001, Status: actions_model.StatusFailure, TokenHash: "admission-pg-enroll-task"}
	require.NoError(t, db.Insert(t.Context(), task))
	ctx, tx, err := db.TxContext(t.Context())
	require.NoError(t, err)
	defer tx.Close()
	require.NoError(t, actions_model.EnrollTaskReceipt(ctx, task))
	pid, result := admissionPostgresDelete(t, func(ctx context.Context) error { return actions_model.DeleteTask(ctx, 20001) })
	admissionPostgresBlocked(t, <-pid)
	require.NoError(t, tx.Commit())
	require.ErrorContains(t, <-result, "awaits runner final-report receipt")
	actual, err := actions_model.GetTaskByID(t.Context(), 20001)
	require.NoError(t, err)
	require.Equal(t, sql.NullBool{Valid: true, Bool: false}, actual.RunnerFinalReportReceived)
}

func TestAdmissionPostgresEnrollmentSerializesRepositoryDeletion(t *testing.T) {
	defer admissionPostgresEnv(t)()
	task := &actions_model.ActionTask{ID: 20002, RepoID: 1, RunnerID: 90002, Status: actions_model.StatusFailure, TokenHash: "admission-pg-enroll-repo"}
	require.NoError(t, db.Insert(t.Context(), task))
	ctx, tx, err := db.TxContext(t.Context())
	require.NoError(t, err)
	defer tx.Close()
	require.NoError(t, actions_model.EnrollTaskReceipt(ctx, task))
	pid, result := admissionPostgresDelete(t, func(ctx context.Context) error {
		return repository_service.DeleteRepositoryDirectly(ctx, 1, repository_service.DeleteRepositoryOpts{})
	})
	admissionPostgresBlocked(t, <-pid)
	require.NoError(t, tx.Commit())
	require.ErrorContains(t, <-result, "pending runner final-report receipts")
	_, err = actions_model.GetTaskByID(t.Context(), 20002)
	require.NoError(t, err)
}

func TestAdmissionPostgresPendingReceiptSurvivesConnectionAndMissingRunner(t *testing.T) {
	defer admissionPostgresEnv(t)()
	require.NoError(t, db.Insert(t.Context(), &actions_model.ActionTask{ID: 20003, RepoID: 1, RunnerID: 90003, Status: actions_model.StatusCancelled, TokenHash: "admission-pg-pending-missing-runner", RunnerFinalReportReceived: sql.NullBool{Valid: true, Bool: false}}))
	pending, uncovered, nonterminal, err := actions_model.TaskReceiptCounts(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), pending)
	require.Equal(t, int64(4), uncovered)
	require.Equal(t, int64(4), nonterminal)
	require.NoError(t, db.WithTx(t.Context(), func(ctx context.Context) error {
		pending, _, _, err := actions_model.TaskReceiptCounts(ctx)
		if err != nil {
			return err
		}
		require.Equal(t, int64(1), pending)
		return nil
	}))
}

func admissionPostgresAssignmentFixture(t *testing.T) *actions_model.ActionRunner {
	t.Helper()
	require.NoError(t, db.Insert(t.Context(), &actions_model.ActionRun{ID: 20004, RepoID: 1, OwnerID: 2, Status: actions_model.StatusWaiting, Event: "push", CommitSHA: "65f1bf27bc3bf70f64657658635e66094edbcb4d"}))
	require.NoError(t, db.Insert(t.Context(), &actions_model.ActionRunJob{ID: 20004, RunID: 20004, RepoID: 1, OwnerID: 2, Status: actions_model.StatusWaiting, Attempt: 1, Name: "admission-pg", JobID: "proof", RunsOn: []string{"ubuntu-latest"}, WorkflowPayload: []byte("jobs:\n  proof:\n    runs-on: ubuntu-latest\n    steps:\n      - run: 'true'\n")}))
	runner := &actions_model.ActionRunner{ID: 90004, RepoID: 1, Name: "admission-pg", AgentLabels: []string{"ubuntu-latest"}}
	require.NoError(t, db.Insert(t.Context(), runner))
	return runner
}

func TestAdmissionPostgresAssignmentSerializesRepositoryDeletion(t *testing.T) {
	defer admissionPostgresEnv(t)()
	runner := admissionPostgresAssignmentFixture(t)
	ctx, tx, err := db.TxContext(t.Context())
	require.NoError(t, err)
	defer tx.Close()
	task, err := actions_model.CreateTaskForRunner(ctx, runner, nil, nil)
	require.NoError(t, err)
	require.Equal(t, sql.NullBool{Valid: true, Bool: false}, task.RunnerFinalReportReceived)
	pid, result := admissionPostgresDelete(t, func(ctx context.Context) error {
		return repository_service.DeleteRepositoryDirectly(ctx, 1, repository_service.DeleteRepositoryOpts{})
	})
	admissionPostgresBlocked(t, <-pid)
	require.NoError(t, tx.Commit())
	require.ErrorContains(t, <-result, "pending runner final-report receipts")
	actual, err := actions_model.GetTaskByID(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, sql.NullBool{Valid: true, Bool: false}, actual.RunnerFinalReportReceived)
}

func TestAdmissionPostgresAssignmentRollbackDoesNotLeavePending(t *testing.T) {
	defer admissionPostgresEnv(t)()
	runner := admissionPostgresAssignmentFixture(t)
	ctx, tx, err := db.TxContext(t.Context())
	require.NoError(t, err)
	task, err := actions_model.CreateTaskForRunner(ctx, runner, nil, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Close())
	_, err = actions_model.GetTaskByID(t.Context(), task.ID)
	require.Error(t, err)
	pending, _, _, err := actions_model.TaskReceiptCounts(t.Context())
	require.NoError(t, err)
	require.Zero(t, pending)
}

func TestAdmissionPostgresTaskDeletionBeforeEnrollmentRefusesDelivery(t *testing.T) {
	defer admissionPostgresEnv(t)()
	task := &actions_model.ActionTask{ID: 20005, RepoID: 1, RunnerID: 90005, Status: actions_model.StatusFailure, TokenHash: "admission-pg-delete-before-enroll"}
	require.NoError(t, db.Insert(t.Context(), task))
	ctx, tx, err := db.TxContext(t.Context())
	require.NoError(t, err)
	defer tx.Close()
	require.NoError(t, actions_model.DeleteTask(ctx, 20005))
	pid, result := admissionPostgresDelete(t, func(ctx context.Context) error { return actions_model.EnrollTaskReceipt(ctx, task) })
	admissionPostgresBlocked(t, <-pid)
	require.NoError(t, tx.Commit())
	require.Error(t, <-result, "a deleted legacy task must not be returned as enrolled recovery")
}
