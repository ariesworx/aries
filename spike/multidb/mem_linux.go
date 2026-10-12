package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readProc reads a process's memory from /proc/PID/status, where the
// kernel splits the resident set into anonymous and file-backed pages.
func readProc(pid int) (procMem, error) {
	m := procMem{rss: -1, anon: -1, file: -1, swap: -1}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return m, err
	}
	kib := fields(string(b), ":")
	get := func(name string) int64 {
		v, ok := kib[name]
		if !ok {
			return -1
		}
		return v << 10
	}
	m.rss, m.anon, m.swap = get("VmRSS"), get("RssAnon"), get("VmSwap")
	// Shared memory is file-backed to the kernel; Dolt maps none to speak of.
	if f, sh := get("RssFile"), get("RssShmem"); f >= 0 && sh >= 0 {
		m.file = f + sh
	}
	if m.rss < 0 {
		return m, fmt.Errorf("no VmRSS in /proc/%d/status", pid)
	}
	return m, nil
}

// fields reads lines of "name<sep> number ..." into a map.
func fields(s, sep string) map[string]int64 {
	m := map[string]int64{}
	for line := range strings.SplitSeq(s, "\n") {
		name, rest, ok := strings.Cut(line, sep)
		if f := strings.Fields(rest); ok && len(f) > 0 {
			if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				m[name] = v
			}
		}
	}
	return m
}

const cgroupDir = "/sys/fs/cgroup/"

// readCgroup reads this process's cgroup (v2), as a container sees its
// own at the root of the cgroup filesystem. It reports false where there
// is none to read, as on a host's root cgroup.
func readCgroup() (cgroup, bool) {
	var c cgroup
	cur, err := os.ReadFile(cgroupDir + "memory.current")
	if err != nil {
		return c, false
	}
	c.current, _ = strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64)
	if b, err := os.ReadFile(cgroupDir + "memory.max"); err == nil {
		c.max, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64) // "max" stays 0
	}
	if b, err := os.ReadFile(cgroupDir + "memory.stat"); err == nil {
		st := fields(string(b), " ")
		c.anon, c.file, c.kernel = st["anon"], st["file"], st["kernel"]
	}
	if b, err := os.ReadFile(cgroupDir + "memory.events"); err == nil {
		c.oomKills = fields(string(b), " ")["oom_kill"]
	}
	return c, true
}

// system describes the kernel and what the container may use.
func system() string {
	var b strings.Builder
	for _, f := range []string{"/proc/version", cgroupDir + "memory.max", cgroupDir + "memory.swap.max", cgroupDir + "cpu.max"} {
		if v, err := os.ReadFile(f); err == nil {
			fmt.Fprintf(&b, "%s: %s\n", f, strings.TrimSpace(string(v)))
		}
	}
	if v, err := os.ReadFile("/proc/meminfo"); err == nil {
		m := fields(string(v), ":")
		fmt.Fprintf(&b, "/proc/meminfo: MemTotal %d MiB, MemAvailable %d MiB, SwapTotal %d MiB\n", m["MemTotal"]>>10, m["MemAvailable"]>>10, m["SwapTotal"]>>10)
	}
	fmt.Fprintf(&b, "load average: %s\n", loadavg())
	return b.String()
}

// loadavg is the system's load over 1, 5 and 15 minutes: how busy the
// machine was with everything, not only this run.
func loadavg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}
	return strings.Join(strings.Fields(string(b))[:3], " ")
}
