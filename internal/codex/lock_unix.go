// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package codex

import (
	"os"
	"syscall"
)

func acquireFileLock(path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		syscall.Close(fd)
		return nil, &os.PathError{Op: "flock", Path: path, Err: err}
	}
	return func() { syscall.Close(fd) }, nil
}
