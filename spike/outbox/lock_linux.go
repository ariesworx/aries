package main

// Open file description locks, from Linux's <fcntl.h>. The syscall package
// does not name them.
const (
	ofdSetLk  = 37 // F_OFD_SETLK
	ofdSetLkW = 38 // F_OFD_SETLKW
)
