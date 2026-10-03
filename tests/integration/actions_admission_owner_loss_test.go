// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package integration

import (
	"bufio"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/unittest"
	user_model "forgejo.org/models/user"
	actions_module "forgejo.org/modules/actions"
	"forgejo.org/modules/setting"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestActionAdmissionOwnerDeath(t *testing.T) {
	if runtime.GOOS != "linux" || !setting.Database.Type.IsSQLite3() {
		t.Skip("native Linux owner-process HTTP proof requires SQLite")
	}
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		path := filepath.Join(t.TempDir(), "admission.lock")
		require.NoError(t, os.WriteFile(path, []byte("open\n"), 0o600))
		require.NoError(t, actions_module.InitAdmissionLock(path))
		t.Cleanup(func() { require.NoError(t, actions_module.InitAdmissionLock("")) })
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := createFetchTaskTestRepository(t, user, "owner.yml", `
on:
  push:
jobs:
  job:
    strategy:
      matrix:
        version: [a, b]
    runs-on: ubuntu-latest
    steps:
      - run: echo owner
`)
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, user.Name, repo.Name, "owner-loss-test", []string{"ubuntu-latest"})
		runner.setRequestKey("6b685da9-8140-4c14-9f07-c24492851f8d")
		assigned := runner.fetchTask(t)
		owner := exec.Command("python3", "-c", `import fcntl, os, sys, time
f = open(sys.argv[1], "r+b", buffering=0)
fcntl.flock(f, fcntl.LOCK_EX)
f.write(b"paused\n")
f.truncate(7)
os.fsync(f.fileno())
print("READY", flush=True)
time.sleep(3600)
`, path)
		ready, err := owner.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, owner.Start())
		t.Cleanup(func() { _ = owner.Process.Kill() })
		line, err := bufio.NewReader(ready).ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "READY\n", line)
		require.NoError(t, owner.Process.Kill())
		require.Error(t, owner.Wait())
		// Recovery and final reporting still work after the real EX owner has died.
		recovered := runner.maybeFetchTask(t)
		require.NotNil(t, recovered)
		require.Equal(t, assigned.Id, recovered.Id)
		logResponse, err := runner.client.runnerServiceClient.UpdateLog(t.Context(), connect.NewRequest(&runnerv1.UpdateLogRequest{
			TaskId: assigned.Id, NoMore: true,
			Rows: []*runnerv1.LogRow{{Time: timestamppb.Now(), Content: "final log after owner death"}},
		}))
		require.NoError(t, err)
		require.Equal(t, int64(1), logResponse.Msg.AckIndex)
		_, err = runner.client.runnerServiceClient.UpdateTask(t.Context(), connect.NewRequest(&runnerv1.UpdateTaskRequest{
			State:   &runnerv1.TaskState{Id: assigned.Id, Result: runnerv1.Result_RESULT_SUCCESS, StoppedAt: timestamppb.Now()},
			Outputs: map[string]string{"accepted": "after-owner-death"},
		}))
		require.NoError(t, err)
		task, err := actions_model.GetTaskByID(t.Context(), assigned.Id)
		require.NoError(t, err)
		require.True(t, task.RunnerFinalReportReceived.Bool)
		runner.setRequestKey("e6a1826c-5e2a-465f-8f61-d93a46a1a421")
		newTask, additional, err := runner.fetchTaskOrError(t, 2)
		require.NoError(t, err)
		require.Nil(t, newTask)
		require.Empty(t, additional)
		runner.lastTasksVersion = 0
		require.Nil(t, runner.maybeFetchSingleTask(t, nil))
		snapshot, err := actions_module.AdmissionSnapshot(task.RunnerID)
		require.NoError(t, err)
		require.True(t, snapshot.Fenced)
		require.Zero(t, snapshot.LastFetch.TaskCount)
		// Interrupted persisted state must neither assign work nor become an idle witness.
		cachedVersion := runner.lastTasksVersion
		for _, state := range []string{"", "open", "paused", "open\nextra", "unknown\n"} {
			require.NoError(t, os.WriteFile(path, []byte(state), 0o600))
			runner.lastTasksVersion = cachedVersion
			cached, extra, err := runner.fetchTaskOrError(t, 2)
			require.NoError(t, err) // An unchanged-version empty response performs no assignment.
			require.Nil(t, cached)
			require.Empty(t, extra)
			runner.lastTasksVersion = 0 // Force the assignment path instead of the unchanged-version fast return.
			_, _, err = runner.fetchTaskOrError(t, 2)
			require.ErrorContains(t, err, "state must be exactly")
			_, err = actions_module.AdmissionSnapshot(task.RunnerID)
			require.ErrorContains(t, err, "state must be exactly")
		}
		// An authorized writer reopens under EX on the unchanged inode.
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		_, err = file.WriteAt([]byte("open\n"), 0)
		require.NoError(t, err)
		require.NoError(t, file.Truncate(5))
		require.NoError(t, file.Sync())
		require.NoError(t, file.Close())
		runner.lastTasksVersion = 0
		require.NotNil(t, runner.maybeFetchTask(t))
	})
}
