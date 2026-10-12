//go:build !linux

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// readProc reads a process's resident set from ps, which gives the total
// only. It works on macOS and the BSDs; Windows has no ps, and the reading
// then shows as unknown.
func readProc(pid int) (procMem, error) {
	m := procMem{rss: -1, anon: -1, file: -1, swap: -1}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return m, err
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return m, err
	}
	m.rss = kib << 10
	return m, nil
}

// readCgroup reports that there is no cgroup: they are Linux's.
func readCgroup() (cgroup, bool) { return cgroup{}, false }

// system has nothing to add to the first line outside Linux.
func system() string { return "" }

// loadavg is read on Linux only.
func loadavg() string { return "unknown" }
