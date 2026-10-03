// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package actions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

var ErrAdmissionPaused = errors.New("actions task admission is paused")

var ErrAdmissionNotConfigured = errors.New("actions admission lock is not configured")

var admissionLock atomic.Pointer[AdmissionLock]

type AdmissionLock struct {
	path     string
	identity os.FileInfo
}

func InitAdmissionLock(path string) error {
	if path == "" {
		admissionLock.Store(nil)
		return nil
	}
	lock, err := NewAdmissionLock(path)
	if err != nil {
		return err
	}
	admissionLock.Store(lock)
	return nil
}

func NewAdmissionLock(path string) (*AdmissionLock, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("actions admission lock path must be absolute: %q", path)
	}
	if err := admissionLockSupported(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("actions admission lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("actions admission lock must be a regular file without group or other write permission: %q", path)
	}
	lock := &AdmissionLock{path: path, identity: info}
	file, err := lock.open()
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return lock, nil
}

func (l *AdmissionLock) open() (*os.File, error) {
	info, err := os.Lstat(l.path)
	if err != nil {
		return nil, fmt.Errorf("actions admission lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || !os.SameFile(l.identity, info) {
		return nil, fmt.Errorf("actions admission lock inode changed: %q", l.path)
	}
	file, err := os.Open(l.path)
	if err != nil {
		return nil, fmt.Errorf("actions admission lock: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(l.identity, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("actions admission lock inode changed while opening: %q: %v", l.path, err)
	}
	return file, nil
}

func AcquireAdmission() (func() error, error) {
	lock := admissionLock.Load()
	if lock == nil {
		return func() error { return nil }, nil
	}
	return lock.Acquire()
}

func (l *AdmissionLock) Acquire() (func() error, error) {
	// Each transaction needs its own open file description: flock ownership is shared by duplicated descriptors.
	file, err := l.open()
	if err != nil {
		return nil, err
	}
	if err := lockAdmissionShared(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	info, err := os.Lstat(l.path)
	if err != nil || !os.SameFile(l.identity, info) {
		_ = file.Close()
		return nil, fmt.Errorf("actions admission lock inode changed during acquisition: %q: %v", l.path, err)
	}
	return file.Close, nil
}
