//go:build linux

package storage

import (
	"io/fs"
	"syscall"
	"time"
)

// atime reports when info's file was last accessed.
func atime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Unix())
	}
	return info.ModTime()
}
