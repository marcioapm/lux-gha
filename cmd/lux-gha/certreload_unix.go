//go:build unix

package main

import (
	"os"
	"syscall"
)

// inode identifies the file behind a path, so an atomic replacement that
// keeps the size and modification time is still seen as a change.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is uint32 on some platforms
	}
	return 0
}
