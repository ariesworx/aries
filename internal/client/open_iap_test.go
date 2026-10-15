//go:build unix

package client

import (
	"errors"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/iaptest"
	"github.com/ariesworx/aries/internal/proto"
)

// Through IAP the connection is gcloud, a child process: a Dial that
// gives up on a server gone quiet after it accepted the key kills and
// reaps it, as every other failure does, and names the instance.
func TestDialIAPGivesUpOnAQuietServer(t *testing.T) {
	// Serial: puts a stand-in gcloud on the process's PATH (iaptest.Install), and counts its goroutines (settled).
	d := newQuietSSHD(t, ignoresSession)
	gcloud := iaptest.Install(t, iaptest.Bridge)
	cfg := iapConfig(t, d.port, d.hostKey)
	settled(t)
	ctx, pass := newUntil(t)
	defer pass()
	result := make(chan error, 1)
	go func() {
		c, err := Dial(ctx, cfg, Options{Getenv: noAgent, Timeout: time.Minute})
		if err == nil {
			_ = c.Close()
		}
		result <- err
	}()
	await(t, d.reached, "the server never got the request to open a session")
	pass()
	err := await(t, result, "Dial had not returned, though its time was up")
	const want = "timed out after 1m0s waiting for instance aries-1 (project example-project, zone us-central1-a) to open an ssh session"
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeUnavailable || pe.Message != want || pe.Fix == "" {
		t.Errorf("Dial = %v, want an unavailable *proto.Error with a fix, saying %q", err, want)
	}
	if pid := gcloud.Last(t).PID; !reaped(pid) {
		t.Errorf("gcloud (pid %d) still exists after the failed Dial", pid)
	}
	await(t, d.hungUp, "the server still had the tunnel's connection open after Dial returned")
}
