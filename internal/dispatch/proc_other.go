//go:build !unix

package dispatch

import (
	"errors"
	"os/exec"
)

// errUnsupported: process groups and signals are Unix's; `aries dispatch
// run` refuses before it reaches any of this.
var errUnsupported = errors.New("aries dispatch runs only on macOS and Linux")

func ownGroup(*exec.Cmd) {}

func signalGroup(int, bool) error { return errUnsupported }

func alive(int, string) bool { return false }

// LockInstance refuses: the loop does not run here.
func LockInstance(string) (func(), error) { return nil, errUnsupported }

// Running reports false: no dispatcher runs here.
func Running(string) bool { return false }
