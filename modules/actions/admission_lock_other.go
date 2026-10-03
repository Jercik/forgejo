// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package actions

import (
	"errors"
	"os"
)

func admissionLockSupported() error {
	return errors.New("actions admission lock is unsupported on this operating system")
}
func lockAdmissionShared(_ *os.File) error { return admissionLockSupported() }

func admissionFileIdentity(_ os.FileInfo) (uint64, uint64) { return 0, 0 }
