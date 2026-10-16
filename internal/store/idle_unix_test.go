//go:build linux || darwin

package store

import (
	"net"
	"testing"
	"time"
)

// pgx can leave a background reader blocked in Read on an idle
// connection (draining after a canceled query). idleNews must not wait
// for that read to end: the pool's acquire would hang behind it.
func TestIdleNewsDoesNotWaitForAReader(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server := <-accepted
	defer func() { _ = server.Close() }()

	reading := make(chan struct{})
	go func() {
		close(reading)
		var b [1]byte
		_, _ = client.Read(b[:]) // blocks: the server sends nothing
	}()
	<-reading
	time.Sleep(50 * time.Millisecond) // let the Read take the read lock

	done := make(chan bool, 1)
	go func() { done <- idleNews(client) }()
	select {
	case news := <-done:
		if news {
			t.Errorf("idleNews on a quiet connection with a reader = true, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idleNews waited for another goroutine's Read to end")
	}
}
