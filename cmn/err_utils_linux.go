// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"errors"
	"io"
	"syscall"
)

// Checks if the error is generated by any IO operation and if the error
// is severe enough to run the FSHC for mountpath testing
//
// For mountpath definition, see fs/mountfs.go
func IsIOError(err error) bool {
	if err == nil {
		return false
	}

	ioErrs := []error{
		io.ErrShortWrite,

		syscall.EIO,     // I/O error
		syscall.ENOTDIR, // mountpath is missing
		syscall.EBUSY,   // device or resource is busy
		syscall.ENXIO,   // No such device
		syscall.EBADF,   // Bad file number
		syscall.ENODEV,  // No such device
		syscall.EUCLEAN, // (mkdir)structure needs cleaning = broken filesystem
		syscall.EROFS,   // readonly filesystem
		syscall.EDQUOT,  // quota exceeded
		syscall.ESTALE,  // stale file handle
		syscall.ENOSPC,  // no space left
	}
	for _, ioErr := range ioErrs {
		if errors.Is(err, ioErr) {
			return true
		}
	}
	return false
}