//go:build !unix

package pgtest

// endWithParent does nothing where a process is not told that its parent
// is gone.
func endWithParent(func()) {}
