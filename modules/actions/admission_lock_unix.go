// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package actions

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func admissionLockSupported() error { return nil }

func admissionFileIdentity(info os.FileInfo) (uint64, uint64) {
	stat := info.Sys().(*syscall.Stat_t)
	return uint64(stat.Dev), stat.Ino
}

func lockAdmissionShared(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrAdmissionPaused
	}
	if err != nil {
		return fmt.Errorf("actions admission lock: %w", err)
	}
	return nil
}
