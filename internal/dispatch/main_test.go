package dispatch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/clienttest"
)

// The test binary stands in for aries, and for `aries lock run … --
// claude …`, when started with standinEnv set: it records how it was
// started and answers as the environment says. No test starts the real
// aries or a real harness.
const (
	standinEnv = "ARIES_DISPATCH_STANDIN" // "aries"
	recordEnv  = "ARIES_DISPATCH_RECORD"  // the file each start appends a line to
	// replyPrefix + a command's name in capitals is the JSON it prints,
	// and exitPrefix + the name the code it exits with.
	replyPrefix = "ARIES_DISPATCH_REPLY_"
	exitPrefix  = "ARIES_DISPATCH_EXIT_"
	// runEnv is how `lock run` behaves: ok, auth, failed, refused or hang.
	runEnv = "ARIES_DISPATCH_RUN"
)

func TestMain(m *testing.M) {
	if os.Getenv(standinEnv) == "aries" {
		os.Exit(standinAries())
	}
	restore := clienttest.Isolate()
	restoreGit := clienttest.IsolateGit()
	code := m.Run()
	restoreGit()
	restore()
	os.Exit(code)
}

// started is one start of the stand-in, as it records it.
type started struct {
	Args    []string `json:"args"`
	Session string   `json:"session"`
	Key     string   `json:"key"`
	Stdin   string   `json:"stdin"`
	Dir     string   `json:"dir"`
}

// interrupted hears SIGINT from the stand-in's start, so a run in mode
// hang that has recorded its start is sure to hear it.
var interrupted chan os.Signal

func standinAries() int {
	if os.Getenv(runEnv) == "hang" {
		interrupted = make(chan os.Signal, 1)
		signal.Notify(interrupted, syscall.SIGINT)
	}
	args := os.Args[1:]
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C":
			i++
		case "--json", "--no-queue":
		default:
			rest = append(rest, args[i])
		}
	}
	in, _ := io.ReadAll(os.Stdin)
	wd, _ := os.Getwd()
	rec := started{Args: rest, Session: os.Getenv("ARIES_SESSION"), Key: os.Getenv("ANTHROPIC_API_KEY"), Stdin: string(in), Dir: wd}
	if path := os.Getenv(recordEnv); path != "" {
		b, _ := json.Marshal(rec)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the test's record file
		if err == nil {
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
	if len(rest) >= 2 && rest[0] == "lock" && rest[1] == "run" {
		return standinRun()
	}
	if len(rest) == 0 {
		return 2
	}
	name := strings.ToUpper(rest[0])
	fmt.Print(os.Getenv(replyPrefix + name))
	if c := os.Getenv(exitPrefix + name); c != "" {
		var n int
		_, _ = fmt.Sscan(c, &n)
		return n
	}
	return 0
}

// standinRun is `aries lock run dispatch/runs … -- claude -p …` in the
// mode runEnv names, printing what each prints.
func standinRun() int {
	switch os.Getenv(runEnv) {
	case "ok":
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"PR opened"}`)
		return 0
	case "auth":
		fmt.Println(`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key · Please run /login"}`)
		return 1
	case "failed":
		fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"tool failed"}`)
		return 1
	case "refused":
		fmt.Fprintln(os.Stderr, "aries: dispatch/runs is full: 4 of 4 hold it")
		fmt.Fprintln(os.Stderr, "fix: retry later, or wait with --wait")
		return 1
	case "hang":
		select {
		case <-interrupted:
			return 130
		case <-time.After(time.Minute):
			return 0
		}
	}
	return 3
}
