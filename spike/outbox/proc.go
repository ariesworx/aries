package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

// child is one process the parent started.
type child struct {
	id   int
	role string
	cmd  *exec.Cmd
	done chan struct{}
	err  error // its exit, once done is closed

	mu       sync.Mutex
	killedAt time.Time // when the parent killed it; zero if it did not
}

// spawn starts this binary again as a child. It waits, once it has opened
// what it needs, until release lets every child go.
func spawn(cfg config, role string, id int) (*child, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	c := &child{id: id, role: role, done: make(chan struct{})}
	c.cmd = exec.Command(self, "-child", role, "-id", strconv.Itoa(id), "-cfg", string(b))
	c.cmd.Stderr = os.Stderr
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s %d: %w", role, id, err)
	}
	go func() {
		c.err = c.cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func spawnN(cfg config, role string, n, firstID int) ([]*child, error) {
	kids := make([]*child, 0, n)
	for i := range n {
		c, err := spawn(cfg, role, firstID+i)
		if err != nil {
			killAll(kids)
			return nil, err
		}
		kids = append(kids, c)
	}
	return kids, nil
}

// kill ends the child at once: SIGKILL, or TerminateProcess on Windows. It
// returns the time just before the kill was sent.
func (c *child) kill() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.killedAt.IsZero() {
		return c.killedAt
	}
	c.killedAt = time.Now()
	_ = c.cmd.Process.Kill()
	return c.killedAt
}

func (c *child) killed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.killedAt.IsZero()
}

func (c *child) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func killAll(kids []*child) {
	for _, c := range kids {
		if !c.exited() {
			c.kill()
		}
	}
	for _, c := range kids {
		<-c.done
	}
}

// waitAll waits for every child. It returns how many failed without being
// killed, and how many had to be killed because they outlasted the
// timeout: a child that never finishes is a lock left held.
func waitAll(kids []*child, timeout time.Duration) (failed, stalled int) {
	deadline := time.After(timeout)
	for _, c := range kids {
		select {
		case <-c.done:
		case <-deadline:
			for _, c := range kids {
				if !c.exited() && !c.killed() {
					stalled++
					c.kill()
				}
			}
			<-c.done
		}
	}
	for _, c := range kids {
		if c.err != nil && !c.killed() {
			failed++
		}
	}
	return failed, stalled
}

// newStore makes one experiment's store directory, 0700 as §18.4 has it.
func newStore(base, name string) (string, error) {
	dir, err := os.MkdirTemp(base, name+"-")
	if err != nil {
		return "", err
	}
	for _, sub := range []string{"ready", "res", "ack", "server", "holders", "cache"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// ready is a child saying it is set up; it then waits for the go file.
func ready(dir, role string, id int) error {
	if err := touch(filepath.Join(dir, "ready", fmt.Sprintf("%s-%d", role, id))); err != nil {
		return err
	}
	for !exists(filepath.Join(dir, "go")) {
		time.Sleep(500 * time.Microsecond)
	}
	return nil
}

// release waits until want children are ready and lets them all go. It
// returns when it did.
func release(dir string, want int) (time.Time, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		des, err := os.ReadDir(filepath.Join(dir, "ready"))
		if err != nil {
			return time.Time{}, err
		}
		if len(des) >= want {
			break
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("%d of %d children ready after 2m", len(des), want)
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	return start, touch(filepath.Join(dir, "go"))
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// waitFor polls until path exists.
func waitFor(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !exists(path) {
		if time.Now().After(deadline) {
			return fmt.Errorf("no %s after %v", filepath.Base(path), timeout)
		}
		time.Sleep(200 * time.Microsecond)
	}
	return nil
}

// result is what a child measured: named lists of durations in
// nanoseconds, and named counts.
type result struct {
	Lat map[string][]int64 `json:"lat"`
	N   map[string]int64   `json:"n"`
}

func newResult() *result {
	return &result{Lat: map[string][]int64{}, N: map[string]int64{}}
}

func (r *result) add(name string, d time.Duration) {
	r.Lat[name] = append(r.Lat[name], int64(d))
}

func (r *result) write(dir, role string, id int) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "res", fmt.Sprintf("%s-%d.json", role, id)), b, 0o600)
}

// readResults merges what every child that finished wrote.
func readResults(dir string) (*result, error) {
	all := newResult()
	des, err := os.ReadDir(filepath.Join(dir, "res"))
	if err != nil {
		return nil, err
	}
	for _, de := range des {
		b, err := os.ReadFile(filepath.Join(dir, "res", de.Name()))
		if err != nil {
			return nil, err
		}
		var r result
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", de.Name(), err)
		}
		for k, v := range r.Lat {
			all.Lat[k] = append(all.Lat[k], v...)
		}
		for k, v := range r.N {
			all.N[k] += v
		}
	}
	for _, v := range all.Lat {
		slices.Sort(v)
	}
	return all, nil
}

// pct is the p-th percentile, nearest rank, of sorted durations, in
// milliseconds. p of 1 is the largest.
func pct(sorted []int64, p float64) string {
	if len(sorted) == 0 {
		return "-"
	}
	i := max(int(math.Ceil(p*float64(len(sorted))))-1, 0)
	return ms(time.Duration(sorted[i]))
}

func ms(d time.Duration) string {
	v := float64(d) / float64(time.Millisecond)
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.3f", v)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
