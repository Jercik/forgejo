// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	actions_model "forgejo.org/models/actions"
	auth_model "forgejo.org/models/auth"
	"forgejo.org/models/unittest"
	user_model "forgejo.org/models/user"
	actions_module "forgejo.org/modules/actions"
	"forgejo.org/modules/structs"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
)

func TestActionAdmissionFetchObservation(t *testing.T) {
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		path := filepath.Join(t.TempDir(), "admission.lock")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, actions_module.InitAdmissionLock(path))
		t.Cleanup(func() { require.NoError(t, actions_module.InitAdmissionLock("")) })
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := createFetchTaskTestRepository(t, user, "observation.yml", `
on:
  push:
jobs:
  job:
    runs-on: ubuntu-latest
    steps:
      - run: echo observation
`)
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, user.Name, repo.Name, "observation-test", []string{"ubuntu-latest"})
		runner.setRequestKey("aa39a9fd-682a-4a58-b026-33a848cf804c")
		assigned := runner.fetchTask(t)
		modelRunner, err := actions_model.GetRunnerByUUID(t.Context(), runner.uuid)
		require.NoError(t, err)
		token := getUserToken(t, "user1", auth_model.AccessTokenScopeReadAdmin)
		snapshot := func() *structs.ActionRunnerAdmission {
			request := NewRequest(t, http.MethodGet, fmt.Sprintf("/api/v1/admin/actions/runners/%d", modelRunner.ID)).AddTokenAuth(token)
			response := MakeRequest(t, request, http.StatusOK)
			var exposed structs.ActionRunner
			DecodeJSON(t, response, &exposed)
			require.NotNil(t, exposed.Admission)
			return exposed.Admission
		}
		beforePause := snapshot()
		require.False(t, beforePause.Fenced)
		require.Equal(t, uint64(1), beforePause.LastFetch.TaskCount)
		lease, err := os.Open(path)
		require.NoError(t, err)
		defer lease.Close()
		require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		baseline := snapshot()
		require.True(t, baseline.Fenced)
		// Recovery under the fence can return a task while advertising full capacity.
		recovered := runner.maybeFetchTaskWithTaskCapacity
		recoveredTask, extra := recovered(t, 3)
		require.Equal(t, assigned.Id, recoveredTask.Id)
		require.Empty(t, extra)
		observed := snapshot()
		require.Greater(t, observed.LastFetch.Sequence, baseline.FetchSequence)
		require.Equal(t, uint64(1), observed.LastFetch.TaskCount)
		require.True(t, observed.LastFetch.Fenced)
		runner.setRequestKey("c15af438-c35a-4ff0-9b27-732b074f09cb")
		_, _, err = runner.fetchTaskOrError(t, 3)
		require.NoError(t, err)
		observed = snapshot()
		require.Equal(t, uint64(0), observed.LastFetch.TaskCount)
		require.Equal(t, int64(3), *observed.LastFetch.TaskCapacity)
		require.Equal(t, baseline.ProcessID, observed.ProcessID)
		require.Equal(t, baseline.LockInode, observed.LockInode)
		// A successful old-client request with no explicit capacity stays unsuitable for proof.
		_, err = runner.client.runnerServiceClient.FetchTask(t.Context(), connect.NewRequest(&runnerv1.FetchTaskRequest{}))
		require.NoError(t, err)
		require.Nil(t, snapshot().LastFetch.TaskCapacity)
		// Unconfigured servers retain the existing admin response without proof metadata.
		require.NoError(t, actions_module.InitAdmissionLock(""))
		request := NewRequest(t, http.MethodGet, fmt.Sprintf("/api/v1/admin/actions/runners/%d", modelRunner.ID)).AddTokenAuth(token)
		response := MakeRequest(t, request, http.StatusOK)
		var exposed structs.ActionRunner
		DecodeJSON(t, response, &exposed)
		require.Nil(t, exposed.Admission)
	})
}
