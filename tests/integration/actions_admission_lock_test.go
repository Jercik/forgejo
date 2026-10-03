// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package integration

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	"forgejo.org/models/unittest"
	user_model "forgejo.org/models/user"
	actions_module "forgejo.org/modules/actions"
	"forgejo.org/modules/setting"
	actions_service "forgejo.org/services/actions"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/xorm/xorm"
	"code.forgejo.org/xorm/xorm/contexts"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
)

func TestActionAdmissionLockRecovery(t *testing.T) {
	if !setting.Database.Type.IsSQLite3() {
		t.Skip("mock repo runner requires SQLite")
	}
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		path := filepath.Join(t.TempDir(), "admission.lock")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, actions_module.InitAdmissionLock(path))
		t.Cleanup(func() { require.NoError(t, actions_module.InitAdmissionLock("")) })
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := createFetchTaskTestRepository(t, user, "matrix.yml", `
on:
  push:
jobs:
  job:
    strategy:
      matrix:
        version: [a, b]
    runs-on: ubuntu-latest
    steps:
      - run: echo test
`)
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, user.Name, repo.Name, "admission-test", []string{"ubuntu-latest"})
		runner.setRequestKey("72e9a82c-76cc-4e28-b087-482a8236241e")
		assigned := runner.fetchTask(t)
		require.NotNil(t, assigned)
		lease, err := os.Open(path)
		require.NoError(t, err)
		defer lease.Close()
		require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		recovered := runner.maybeFetchTask(t)
		require.NotNil(t, recovered)
		require.Equal(t, assigned.Id, recovered.Id)
		runner.setRequestKey("f97c5c45-b6b4-4e98-b806-928558164f5b")
		runner.lastTasksVersion = 0
		require.Nil(t, runner.maybeFetchTask(t))
		runner.lastTasksVersion = 0
		require.Nil(t, runner.maybeFetchSingleTask(t, nil))
		clients := []*mockRunnerClient{
			newMockRunnerClientWithRequestKey(runner.uuid, runner.token, "67e0caa3-1189-4329-ae2d-85dd4c026d16"),
			newMockRunnerClientWithRequestKey(runner.uuid, runner.token, "1b720a7f-08f7-4180-992d-60fc86de81a0"),
		}
		var requests sync.WaitGroup
		for _, client := range clients {
			requests.Go(func() {
				capacity := int64(2)
				response, err := client.runnerServiceClient.FetchTask(t.Context(), connect.NewRequest(&runnerv1.FetchTaskRequest{TaskCapacity: &capacity}))
				require.NoError(t, err)
				require.Nil(t, response.Msg.Task)
				require.Empty(t, response.Msg.AdditionalTasks)
			})
		}
		requests.Wait()
		require.NoError(t, os.Rename(path, path+".original"))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		_, err = runner.client.runnerServiceClient.FetchTask(t.Context(), connect.NewRequest(&runnerv1.FetchTaskRequest{}))
		require.ErrorContains(t, err, "inode changed")
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Rename(path+".original", path))
		require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_UN))
		runner.lastTasksVersion = 0
		require.NotNil(t, runner.fetchTask(t))
	})
}

type admissionTransactionBarrier struct {
	entered  chan struct{}
	release  chan struct{}
	consumed atomic.Bool
}

func (b *admissionTransactionBarrier) BeforeProcess(c *contexts.ContextHook) (context.Context, error) {
	if strings.HasPrefix(c.SQL, "INSERT INTO") && strings.Contains(c.SQL, "action_task") && b.consumed.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
	// XORM passes the original context to every hook; restore the task its tracing hook requires.
	return (db.TracingHook{}).BeforeProcess(c)
}
func (*admissionTransactionBarrier) AfterProcess(*contexts.ContextHook) error { return nil }

func TestActionAdmissionLockInflightTransaction(t *testing.T) {
	if !setting.Database.Type.IsSQLite3() {
		t.Skip("mock repo runner requires SQLite")
	}
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		path := filepath.Join(t.TempDir(), "admission.lock")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, actions_module.InitAdmissionLock(path))
		t.Cleanup(func() { require.NoError(t, actions_module.InitAdmissionLock("")) })
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := createFetchTaskTestRepository(t, user, "main.yml", `
on:
  push:
jobs:
  job:
    runs-on: ubuntu-latest
    steps:
      - run: echo test
`)
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, user.Name, repo.Name, "inflight-admission-test", []string{"ubuntu-latest"})
		barrier := &admissionTransactionBarrier{entered: make(chan struct{}), release: make(chan struct{})}
		engine := db.GetEngine(db.DefaultContext).(*xorm.Engine)
		engine.AddHook(barrier)
		modelRunner, err := actions_model.GetRunnerByUUID(db.DefaultContext, runner.uuid)
		require.NoError(t, err)
		fetched := make(chan *runnerv1.Task, 1)
		failed := make(chan error, 1)
		go func() {
			task, err := actions_service.PickTask(db.DefaultContext, modelRunner, nil, nil)
			fetched <- task
			failed <- err
		}()
		select {
		case <-barrier.entered:
		case <-fetched:
			t.Fatal("assignment returned before transaction barrier")
		}
		lease, err := os.Open(path)
		require.NoError(t, err)
		defer lease.Close()
		require.ErrorIs(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
		close(barrier.release)
		require.NotNil(t, <-fetched)
		require.NoError(t, <-failed)
		require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	})
}

func TestActionAdmissionLockStartupPaused(t *testing.T) {
	if setting.Actions.AdmissionLockPath == "" {
		t.Skip("requires an externally held configured admission lock")
	}
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := createFetchTaskTestRepository(t, user, "main.yml", `
on:
  push:
jobs:
  job:
    runs-on: ubuntu-latest
    steps:
      - run: echo test
`)
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, user.Name, repo.Name, "startup-admission-test", []string{"ubuntu-latest"})
		unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RepoID: repo.ID, Status: actions_model.StatusWaiting})
		require.Nil(t, runner.maybeFetchTask(t))
		require.Nil(t, runner.maybeFetchSingleTask(t, nil))
	})
}
