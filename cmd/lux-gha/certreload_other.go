//go:build !unix

package main

import "os"

// inode is 0 where no inode is available: changes are seen by modification
// time and size only.
func inode(os.FileInfo) uint64 { return 0 }
