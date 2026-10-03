// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	"forgejo.org/models/unittest"
	actions_module "forgejo.org/modules/actions"
	actions_service "forgejo.org/services/actions"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/forgejo/actions-proto/runner/v1/runnerv1connect"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
)

type straddleService struct {
	Service
	pick func(context.Context, *actions_model.ActionRunner, *string, *string) (*runnerv1.Task, error)
}

func (s *straddleService) FetchTask(ctx context.Context, req *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	return s.fetchTask(ctx, req, s.pick)
}

func TestAdmissionBulkFetchStraddle(t *testing.T) {
	defer unittest.OverrideFixtures("models/actions/TestActionTask_GetAvailableJobsForRunner")()
	require.NoError(t, unittest.PrepareTestDatabase())
	path := filepath.Join(t.TempDir(), "admission.lock")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, actions_module.InitAdmissionLock(path))
	t.Cleanup(func() { require.NoError(t, actions_module.InitAdmissionLock("")) })
	lease, err := os.Open(path)
	require.NoError(t, err)
	defer lease.Close()
	runner := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunner{ID: 73711})
	runner.AgentLabels = []string{"ubuntu-latest"}
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo straddle\n")
	_, err = db.GetEngine(t.Context()).Where("repo_id = ?", runner.RepoID).Cols("workflow_payload", "runs_on").Update(&actions_model.ActionRunJob{WorkflowPayload: workflow, RunsOn: []string{"ubuntu-latest"}})
	require.NoError(t, err)
	calls := 0
	service := &straddleService{pick: func(ctx context.Context, runner *actions_model.ActionRunner, key, handle *string) (*runnerv1.Task, error) {
		calls++
		task, err := actions_service.PickTask(ctx, runner, key, handle)
		if calls == 1 && err == nil {
			// The complete real first transaction has released SH. Acquire EX before the real next PickTask.
			err = syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		}
		return task, err
	}}
	_, handler := runnerv1connect.NewRunnerServiceHandler(service)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handler.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), runnerCtxKey{}, runner)))
	}))
	defer server.Close()
	capacity := int64(2)
	client := runnerv1connect.NewRunnerServiceClient(server.Client(), server.URL)
	response, err := client.FetchTask(t.Context(), connect.NewRequest(&runnerv1.FetchTaskRequest{TaskCapacity: &capacity}))
	require.NoError(t, err)
	require.NotNil(t, response.Msg.Task)
	require.Empty(t, response.Msg.AdditionalTasks)
	require.Equal(t, 2, calls)
	observed, err := actions_module.AdmissionSnapshot(runner.ID)
	require.NoError(t, err)
	require.True(t, observed.LastFetch.Fenced)
	require.Equal(t, uint64(1), observed.LastFetch.TaskCount)
	// A post-baseline whole successful response is empty while EX remains held.
	baseline := observed.FetchSequence
	response, err = client.FetchTask(t.Context(), connect.NewRequest(&runnerv1.FetchTaskRequest{TaskCapacity: &capacity}))
	require.NoError(t, err)
	require.Nil(t, response.Msg.Task)
	observed, err = actions_module.AdmissionSnapshot(runner.ID)
	require.NoError(t, err)
	require.Greater(t, observed.LastFetch.Sequence, baseline)
	require.Zero(t, observed.LastFetch.TaskCount)
}
