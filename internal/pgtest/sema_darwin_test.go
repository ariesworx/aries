package pgtest

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// Postgres on macOS takes its semaphores from System V, in sets of 17
// (sysv_sema.c): 16, and one that holds a magic number, last set by the
// postmaster, so that a later server can tell a set left by one that
// is gone. Each set's key is the next free one after the data
// directory's inode number. A server that is killed frees none of them.
const (
	semsPerSet = 17
	semGetPID  = 4 // GETPID in <sys/sem.h>
	semGetVal  = 5 // GETVAL
	semMagic   = 537
)

// semSet is a System V semaphore set: its key and its id.
type semSet struct{ key, id int }

// semaphoreSets returns the semaphore sets the postmaster pid made for
// the server on data: those along its keys whose magic semaphore that
// process set last. The walk ends at the first key with no set, which
// Postgres would have taken.
func semaphoreSets(t *testing.T, data string, pid int) []semSet {
	t.Helper()
	fi, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	ino := int32(fi.Sys().(*syscall.Stat_t).Ino) //nolint:gosec // key_t is the inode number cut to 32 bits, as Postgres takes it
	var sets []semSet
	for key := ino + 1; key != ino+1000; key++ {
		id, err := semget(key)
		if errors.Is(err, syscall.ENOENT) {
			break
		}
		if err != nil {
			continue // another's, which Postgres stepped over
		}
		if creator, val, ok := semMark(id); ok && creator == pid && val == semMagic {
			sets = append(sets, semSet{key: int(key), id: id})
		}
	}
	return sets
}

// semaphoreSetExists reports whether set is still there: its key still
// names its id.
func semaphoreSetExists(s semSet) bool {
	id, err := semget(int32(s.key)) //nolint:gosec // a key semaphoreSets read as an int32
	return err == nil && id == s.id
}

// semget returns the id of the set key names, without making one.
func semget(key int32) (int, error) {
	id, _, errno := syscall.Syscall(syscall.SYS_SEMGET, uintptr(key), 0, 0) //nolint:gosec // key_t is an int32; the kernel reads only the low 32 bits
	if errno != 0 {
		return 0, errno
	}
	return int(id), nil
}

// semMark returns the process that last changed set id's magic
// semaphore, and its value, if it has one.
func semMark(id int) (pid, val int, ok bool) {
	p, _, errno := syscall.Syscall6(syscall.SYS_SEMCTL, uintptr(id), semsPerSet-1, semGetPID, 0, 0, 0) //nolint:gosec // an id semget returned
	if errno != 0 {
		return 0, 0, false
	}
	v, _, errno := syscall.Syscall6(syscall.SYS_SEMCTL, uintptr(id), semsPerSet-1, semGetVal, 0, 0, 0) //nolint:gosec // as above
	if errno != 0 {
		return 0, 0, false
	}
	return int(p), int(v), true
}
