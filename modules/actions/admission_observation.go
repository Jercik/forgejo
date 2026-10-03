// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package actions

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"forgejo.org/modules/structs"

	"github.com/google/uuid"
)

var (
	admissionProcessID     = uuid.NewString()
	admissionFetchSequence atomic.Uint64
	admissionObservations  = struct {
		sync.Mutex
		byRunner map[int64]structs.ActionRunnerFetchObservation
	}{byRunner: make(map[int64]structs.ActionRunnerFetchObservation)}
)

func AdmissionConfigured() bool { return admissionLock.Load() != nil }

func StartAdmissionFetch() uint64 { return admissionFetchSequence.Add(1) }

func AdmissionSnapshot(runnerID int64) (*structs.ActionRunnerAdmission, error) {
	lock := admissionLock.Load()
	if lock == nil {
		return nil, ErrAdmissionNotConfigured
	}
	file, err := lock.open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	device, inode := admissionFileIdentity(info)
	err = lockAdmissionShared(file)
	fenced := errors.Is(err, ErrAdmissionPaused)
	if err != nil && !fenced {
		return nil, err
	}
	paused, err := admissionStatePaused(file)
	if err != nil {
		return nil, err
	}
	fenced = fenced || paused
	current, err := os.Lstat(lock.path)
	if err != nil || !os.SameFile(lock.identity, current) {
		return nil, fmt.Errorf("actions admission lock inode changed during probe: %q: %v", lock.path, err)
	}
	res := &structs.ActionRunnerAdmission{ProcessID: admissionProcessID, FetchSequence: admissionFetchSequence.Load(), Fenced: fenced, LockDevice: device, LockInode: inode}
	admissionObservations.Lock()
	if last, ok := admissionObservations.byRunner[runnerID]; ok {
		res.LastFetch = &last
	}
	admissionObservations.Unlock()
	return res, nil
}

// CompleteAdmissionFetch runs only after successful assembly of the complete bulk response.
func CompleteAdmissionFetch(runnerID int64, sequence uint64, capacity *int64, total uint64) {
	snapshot, err := AdmissionSnapshot(runnerID)
	if err != nil {
		return
	}
	var requested *int64
	if capacity != nil {
		value := *capacity
		requested = &value
	}
	last := structs.ActionRunnerFetchObservation{Sequence: sequence, TaskCapacity: requested, TaskCount: total, Fenced: snapshot.Fenced}
	admissionObservations.Lock()
	// Slow older requests cannot replace a newer completed observation.
	if previous, ok := admissionObservations.byRunner[runnerID]; !ok || previous.Sequence < sequence {
		admissionObservations.byRunner[runnerID] = last
	}
	admissionObservations.Unlock()
}
