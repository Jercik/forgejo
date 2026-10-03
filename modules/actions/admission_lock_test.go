// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package actions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func newAdmissionTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admission.lock")
	require.NoError(t, os.WriteFile(path, []byte("open\n"), 0o600))
	return path
}

func TestAdmissionLockConcurrentAssignments(t *testing.T) {
	path := newAdmissionTestFile(t)
	lock, err := NewAdmissionLock(path)
	require.NoError(t, err)
	releaseFirst, err := lock.Acquire()
	require.NoError(t, err)
	releaseSecond, err := lock.Acquire()
	require.NoError(t, err)
	lease, err := os.Open(path)
	require.NoError(t, err)
	defer lease.Close()
	require.ErrorIs(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
	require.NoError(t, releaseFirst())
	require.ErrorIs(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
	require.NoError(t, releaseSecond())
	require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	_, err = lock.Acquire()
	require.ErrorIs(t, err, ErrAdmissionPaused)
	require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_UN))
	release, err := lock.Acquire()
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestAdmissionLockRejectsReplacement(t *testing.T) {
	path := newAdmissionTestFile(t)
	lock, err := NewAdmissionLock(path)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".held"))
	require.NoError(t, os.WriteFile(path, []byte("open\n"), 0o600))
	_, err = lock.Acquire()
	require.ErrorContains(t, err, "inode changed")
}

func TestAdmissionLockRejectsUnsafePaths(t *testing.T) {
	_, err := NewAdmissionLock("relative.lock")
	require.ErrorContains(t, err, "must be absolute")
	path := newAdmissionTestFile(t)
	require.NoError(t, os.Chmod(path, 0o666))
	_, err = NewAdmissionLock(path)
	require.ErrorContains(t, err, "without group or other write")
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.Symlink(path, path+".link"))
	_, err = NewAdmissionLock(path + ".link")
	require.ErrorContains(t, err, "regular file")
}

func TestAdmissionLockHeldAcrossProcessRestart(t *testing.T) {
	path := newAdmissionTestFile(t)
	lease, err := os.Open(path)
	require.NoError(t, err)
	defer lease.Close()
	require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	first := exec.Command(os.Args[0], "-test.run=^TestAdmissionLockProcessProbe$")
	first.Env = append(os.Environ(), "FORGEJO_TEST_ADMISSION_LOCK="+path)
	output, err := first.CombinedOutput()
	require.NoError(t, err, string(output))
	second := exec.Command(os.Args[0], "-test.run=^TestAdmissionLockProcessProbe$")
	second.Env = append(os.Environ(), "FORGEJO_TEST_ADMISSION_LOCK="+path)
	output, err = second.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestAdmissionLockProcessProbe(t *testing.T) {
	path := os.Getenv("FORGEJO_TEST_ADMISSION_LOCK")
	if path == "" {
		t.Skip("subprocess probe")
	}
	lock, err := NewAdmissionLock(path)
	require.NoError(t, err)
	_, err = lock.Acquire()
	require.ErrorIs(t, err, ErrAdmissionPaused)
}

func TestAdmissionStateSurvivesOwnerExit(t *testing.T) {
	path := newAdmissionTestFile(t)
	require.NoError(t, InitAdmissionLock(path))
	t.Cleanup(func() { require.NoError(t, InitAdmissionLock("")) })
	owner, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	_, err = owner.WriteAt([]byte("paused\n"), 0)
	require.NoError(t, err)
	require.NoError(t, owner.Truncate(7))
	require.NoError(t, owner.Sync())
	// Configured startup accepts a valid paused state while the owner still holds EX.
	require.NoError(t, InitAdmissionLock(path))
	require.NoError(t, owner.Close())
	_, err = AcquireAdmission()
	require.ErrorIs(t, err, ErrAdmissionPaused)
	state, err := AdmissionSnapshot(111)
	require.NoError(t, err)
	require.True(t, state.Fenced)
	require.NoError(t, InitAdmissionLock(path))
	owner, err = os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	_, err = owner.WriteAt([]byte("open\n"), 0)
	require.NoError(t, err)
	require.NoError(t, owner.Truncate(5))
	require.NoError(t, owner.Sync())
	require.NoError(t, owner.Close())
	release, err := AcquireAdmission()
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestAdmissionStateRejectsInterruptedWrites(t *testing.T) {
	path := newAdmissionTestFile(t)
	require.NoError(t, InitAdmissionLock(path))
	t.Cleanup(func() { require.NoError(t, InitAdmissionLock("")) })
	for _, state := range []string{"", "o", "open", "paused", "open\nextra", "paused\nextra", "unknown\n", strings.Repeat("x", 4096)} {
		require.NoError(t, os.WriteFile(path, []byte(state), 0o600))
		_, err := AcquireAdmission()
		require.ErrorContains(t, err, "state must be exactly")
		_, err = AdmissionSnapshot(111)
		require.ErrorContains(t, err, "state must be exactly")
		_, err = NewAdmissionLock(path)
		require.ErrorContains(t, err, "state must be exactly")
	}
}
