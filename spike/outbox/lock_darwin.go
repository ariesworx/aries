package main

// Open file description locks, from macOS's <sys/fcntl.h>, where they are
// behind a private define. The syscall package does not name them, and
// Apple does not document them; the locks experiment says what they do.
const (
	ofdSetLk  = 90 // F_OFD_SETLK
	ofdSetLkW = 91 // F_OFD_SETLKW
)
