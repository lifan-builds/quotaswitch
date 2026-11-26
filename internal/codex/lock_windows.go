// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

//go:build windows

package codex

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const lockLength = 1

func acquireFileLock(path string) (func(), error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(file.Fd())
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, lockLength, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, &os.PathError{Op: "LockFileEx", Path: path, Err: err}
	}
	return func() {
		windows.UnlockFileEx(handle, 0, lockLength, 0, &windows.Overlapped{})
		file.Close()
	}, nil
}
