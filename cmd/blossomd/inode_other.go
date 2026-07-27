//go:build !unix

package main

import "io/fs"

// inodeOf has no portable equivalent off Unix. Returning zero leaves listings in
// readdir order, which costs locality but is otherwise correct.
func inodeOf(fs.FileInfo) uint64 { return 0 }
