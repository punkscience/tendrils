//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// inodeOf returns the file's inode number, used to order a listing by
// approximate physical layout. Zero when unavailable, which just means the
// caller's sort degrades to stable-but-arbitrary.
func inodeOf(info fs.FileInfo) uint64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Ino)
}
