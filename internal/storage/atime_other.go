//go:build !linux && !darwin

package storage

import (
	"io/fs"
	"time"
)

// atime falls back to mtime where no atime helper exists.
//
// ponytail: mtime makes eviction FIFO by write time on these platforms; add a
// build-tagged helper for the OS if it ever runs groxpi in production.
func atime(info fs.FileInfo) time.Time { return info.ModTime() }
