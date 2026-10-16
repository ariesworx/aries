package pgtest

import (
	"os"
	"syscall"
)

// Postgres on macOS takes its semaphores from System V, in sets of 17
// (sysv_sema.c): 16, and one that holds a magic number, last set by the
// postmaster, so that a later server can tell a set left by one that is
// gone. Each set's key is the next free one after the data directory's
// inode number. A server that exits frees its sets; one that is killed
// frees none, and macOS allows 87,381 semaphores in all
// (kern.sysv.semmns), about 400 to a server.
const (
	semsPerSet = 17
	semGetPID  = 4 // GETPID in <sys/sem.h>
	semGetVal  = 5 // GETVAL
	ipcRMID    = 0 // IPC_RMID in <sys/ipc.h>
	semMagic   = 537
	// semKeys is how many keys after a server's first the walk reads.
	// A server of pgtest's settings has 24 sets, but servers started
	// together have data directories with inode numbers a few apart,
	// so their keys interleave: each steps over the sets of the others,
	// and a key free now may have been taken when the server started.
	semKeys = 4096
)

// semaphoreSets returns the semaphore sets the postmaster pid made for
// the server on data: those among its keys whose magic semaphore that
// process set last, which is how Postgres itself tells a set's owner.
func semaphoreSets(data string, pid int) []semSet {
	fi, err := os.Stat(data)
	if err != nil {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	ino := int32(st.Ino) //nolint:gosec // key_t is the inode number cut to 32 bits, as Postgres takes it
	var sets []semSet
	for key := ino + 1; key != ino+semKeys; key++ {
		id, err := semget(key)
		if err != nil {
			continue // none, or another's
		}
		if creator, val, ok := semMark(id); ok && creator == pid && val == semMagic {
			sets = append(sets, semSet{key: key, id: id})
		}
	}
	return sets
}

// semaphoreSetExists reports whether set is still there: its key still
// names its id.
func semaphoreSetExists(s semSet) bool {
	id, err := semget(s.key)
	return err == nil && id == s.id
}

// freeSemaphores removes the semaphore sets the postmaster pid made for
// the server on data, once it has exited: none if it exited by itself,
// all of them if it was killed.
func freeSemaphores(data string, pid int) {
	for _, s := range semaphoreSets(data, pid) {
		_, _, _ = syscall.Syscall6(syscall.SYS_SEMCTL, uintptr(s.id), 0, ipcRMID, 0, 0, 0) //nolint:gosec // an id semget returned
	}
}

// semget returns the id of the set key names, without making one.
func semget(key int32) (int, error) {
	id, _, errno := syscall.Syscall(syscall.SYS_SEMGET, uintptr(key), 0, 0) //nolint:gosec // key_t is an int32; the kernel reads only its low 32 bits
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
