// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package integration

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	"forgejo.org/models/unittest"
	user_model "forgejo.org/models/user"
	actions_service "forgejo.org/services/actions"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/xorm/xorm"
	"code.forgejo.org/xorm/xorm/contexts"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestActionAdmissionFinalReceipt(t *testing.T) {
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := createFetchTaskTestRepository(t, user, "receipt.yml", `
on:
  push:
jobs:
  job:
    runs-on: ubuntu-latest
    steps:
      - run: echo receipt
`)
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, user.Name, repo.Name, "receipt-test", []string{"ubuntu-latest"})
		runner.setRequestKey("ff404124-680a-44e3-8208-f637f427d928")
		assigned := runner.fetchTask(t)
		task, err := actions_model.GetTaskByID(t.Context(), assigned.Id)
		require.NoError(t, err)
		require.True(t, task.RunnerFinalReportReceived.Valid)
		require.False(t, task.RunnerFinalReportReceived.Bool)
		// A server-generated terminal status neither accepts a runner report nor permits deletion.
		require.NoError(t, actions_service.StopTask(t.Context(), task.ID, actions_model.StatusFailure))
		require.ErrorContains(t, actions_model.DeleteTask(t.Context(), task.ID), "awaits runner final-report")
		fault := &receiptOutputFault{}
		db.GetEngine(db.DefaultContext).(*xorm.Engine).AddHook(fault)
		t.Cleanup(func() { fault.enabled.Store(false) })
		state := &runnerv1.TaskState{Id: task.ID, Result: runnerv1.Result_RESULT_FAILURE, StoppedAt: timestamppb.Now()}
		fault.table.Store("action_task_output")
		for _, query := range []string{"INSERT", "SELECT"} {
			fault.query.Store(query)
			fault.enabled.Store(true)
			_, err := runner.client.runnerServiceClient.UpdateTask(t.Context(), connect.NewRequest(&runnerv1.UpdateTaskRequest{State: state, Outputs: map[string]string{"injected": "value"}}))
			require.NoError(t, err) // upstream permits partial output acknowledgment for retry
			fault.enabled.Store(false)
			task, err = actions_model.GetTaskByID(t.Context(), task.ID)
			require.NoError(t, err)
			require.False(t, task.RunnerFinalReportReceived.Bool)
		}
		fault.query.Store("UPDATE")
		fault.table.Store("runner_final_report_received")
		fault.enabled.Store(true)
		_, err = runner.client.runnerServiceClient.UpdateTask(t.Context(), connect.NewRequest(&runnerv1.UpdateTaskRequest{State: state}))
		require.ErrorContains(t, err, "record final report")
		fault.enabled.Store(false)
		task, err = actions_model.GetTaskByID(t.Context(), task.ID)
		require.NoError(t, err)
		require.False(t, task.RunnerFinalReportReceived.Bool)
		oversized := strings.Repeat("k", 256)
		response, err := runner.client.runnerServiceClient.UpdateTask(t.Context(), connect.NewRequest(&runnerv1.UpdateTaskRequest{State: state, Outputs: map[string]string{oversized: "value"}}))
		require.NoError(t, err)
		require.NotContains(t, response.Msg.SentOutputs, oversized)
		task, err = actions_model.GetTaskByID(t.Context(), task.ID)
		require.NoError(t, err)
		require.False(t, task.RunnerFinalReportReceived.Bool)
		// A later terminal retry is processed even though the DB status was already final.
		response, err = runner.client.runnerServiceClient.UpdateTask(t.Context(), connect.NewRequest(&runnerv1.UpdateTaskRequest{State: state, Outputs: map[string]string{"accepted": "value"}}))
		require.NoError(t, err)
		require.Contains(t, response.Msg.SentOutputs, "accepted")
		task, err = actions_model.GetTaskByID(t.Context(), task.ID)
		require.NoError(t, err)
		require.True(t, task.RunnerFinalReportReceived.Bool)
		// Old-key recovery must not erase durable acceptance.
		recovered := runner.maybeFetchTask(t)
		require.NotNil(t, recovered)
		task, err = actions_model.GetTaskByID(t.Context(), task.ID)
		require.NoError(t, err)
		require.True(t, task.RunnerFinalReportReceived.Bool)
		// A deleted runner identity cannot hide a pending row from the global census.
		_, err = db.GetEngine(t.Context()).Exec("UPDATE `action_task` SET runner_id = ?, runner_final_report_received = ? WHERE id = ?", 999999999, false, task.ID)
		require.NoError(t, err)
		pending, _, _, err := actions_model.TaskReceiptCounts(t.Context())
		require.NoError(t, err)
		require.Positive(t, pending)
	})
}

type receiptOutputFault struct {
	enabled atomic.Bool
	query   atomic.Value
	table   atomic.Value
}

func (f *receiptOutputFault) BeforeProcess(c *contexts.ContextHook) (context.Context, error) {
	if f.enabled.Load() && strings.HasPrefix(c.SQL, f.query.Load().(string)) && strings.Contains(c.SQL, f.table.Load().(string)) {
		return c.Ctx, errors.New("injected final-output database fault")
	}
	// Xorm supplies the original context to every hook; preserve the repository's required tracing task.
	return (db.TracingHook{}).BeforeProcess(c)
}

func (*receiptOutputFault) AfterProcess(*contexts.ContextHook) error { return nil }
