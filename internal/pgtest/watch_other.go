//go:build !unix

package pgtest

import "os/exec"

// watcher does nothing where there is no POSIX shell and no process
// group: this package builds there, for the tests that import it, but a
// server it starts can outlive its process.
type watcher struct{}

func startWatcher(string) (*watcher, error) { return &watcher{}, nil }

func (*watcher) watch(int) error { return nil }

func (*watcher) dismiss() {}

func ownGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func quitGroup(*exec.Cmd) {}

func removeSegment(string) error { return nil }
