// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package actions

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdmissionObservationCapacityAndOrder(t *testing.T) {
	path := newAdmissionTestFile(t)
	require.NoError(t, InitAdmissionLock(path))
	t.Cleanup(func() { require.NoError(t, InitAdmissionLock("")) })
	lease, err := os.Open(path)
	require.NoError(t, err)
	defer lease.Close()
	require.NoError(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	baseline, err := AdmissionSnapshot(123456)
	require.NoError(t, err)
	require.True(t, baseline.Fenced)
	first := StartAdmissionFetch()
	second := StartAdmissionFetch()
	capacity := int64(3)
	CompleteAdmissionFetch(123456, second, &capacity, 1)
	CompleteAdmissionFetch(123456, first, &capacity, 0)
	last, err := AdmissionSnapshot(123456)
	require.NoError(t, err)
	require.Greater(t, last.LastFetch.Sequence, baseline.FetchSequence)
	require.Equal(t, uint64(1), last.LastFetch.TaskCount)
	require.Equal(t, capacity, *last.LastFetch.TaskCapacity)
	// Absent wire capacity cannot become an implicit capacity proof.
	CompleteAdmissionFetch(123456, StartAdmissionFetch(), nil, 0)
	last, err = AdmissionSnapshot(123456)
	require.NoError(t, err)
	require.Nil(t, last.LastFetch.TaskCapacity)
	// Changing the inode invalidates API proof rather than returning a stale observation.
	require.NoError(t, os.Rename(path, path+".old"))
	require.NoError(t, os.WriteFile(path, []byte("open\n"), 0o600))
	_, err = AdmissionSnapshot(123456)
	require.ErrorContains(t, err, "inode changed")
}

func TestAdmissionObservationUnconfigured(t *testing.T) {
	require.NoError(t, InitAdmissionLock(""))
	CompleteAdmissionFetch(123456, StartAdmissionFetch(), nil, 0)
	snapshot, err := AdmissionSnapshot(123456)
	require.ErrorIs(t, err, ErrAdmissionNotConfigured)
	require.Nil(t, snapshot)
}
