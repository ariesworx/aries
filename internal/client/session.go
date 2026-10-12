package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/ariesworx/starfix/internal/proto"
)

// SessionEnv lists the environment variables that name the harness session,
// most specific first. STARFIX_SESSION is set by hand or by a harness's
// MCP config; CLAUDE_CODE_SESSION_ID is set by Claude Code for the
// processes it starts; CLAUDE_SESSION_ID is the older spelling. Codex and
// Gemini CLI set no session variable that starfix knows of: the CLI then
// uses CLISession, and `sfx mcp` picks one per process.
var SessionEnv = []string{"STARFIX_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"}

// SessionFromEnv returns the first non-empty variable of SessionEnv, or ""
// to let the server assign a session id.
func SessionFromEnv(getenv func(string) string) string {
	for _, k := range SessionEnv {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// The environment variables that carry a W3C trace context into a
// process, by the OpenTelemetry convention for environment carriers.
const (
	TraceparentEnv = "TRACEPARENT"
	TracestateEnv  = "TRACESTATE"
)

// TraceFromEnv returns the trace context the process was started with,
// from TRACEPARENT and TRACESTATE, or the zero value when there is none
// or it is not a valid one ([proto.ParseTraceContext]): the environment
// is checked as a request frame is, and what does not parse is not sent.
func TraceFromEnv(getenv func(string) string) proto.TraceContext {
	tc, _ := proto.ParseTraceContext(getenv(TraceparentEnv), getenv(TracestateEnv))
	return tc
}

// traceKey is the context key of a call's own trace context.
type traceKey struct{}

// WithTrace returns ctx carrying tc for the calls made under it: a
// [Conn.Call] under the returned context sends tc on its request in
// place of the connection's own ([Options.Trace]). It is how a process
// that serves many pieces of work on one connection, as `sfx mcp` does,
// gives each its caller's trace. The zero value carries nothing, so the
// connection's is sent.
func WithTrace(ctx context.Context, tc proto.TraceContext) context.Context {
	if tc == (proto.TraceContext{}) {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, tc)
}

// TraceFrom returns the trace context [WithTrace] put on ctx.
func TraceFrom(ctx context.Context) (proto.TraceContext, bool) {
	tc, ok := ctx.Value(traceKey{}).(proto.TraceContext)
	return tc, ok
}

// HarnessFromEnv names the agent harness this process runs under, as an
// `sfx setup` agent name, for the agents registry: STARFIX_HARNESS (which
// setup writes into every MCP config) wins; else CLAUDECODE=1 means
// claude-code and GEMINI_CLI=1 means gemini, the markers those harnesses
// set for the commands they run; else "".
func HarnessFromEnv(getenv func(string) string) string {
	switch {
	case getenv("STARFIX_HARNESS") != "":
		return getenv("STARFIX_HARNESS")
	case getenv("CLAUDECODE") == "1":
		return "claude-code"
	case getenv("GEMINI_CLI") == "1":
		return "gemini"
	}
	return ""
}

// CLISession is the session of a person's own `sfx` commands: one per
// principal and machine, so a claim taken in one command is the same
// session's in the next. The server lets only this session renew every
// session's claims (`sfx away`).
const CLISession = proto.CLISession

// NewSessionID returns a random session id, "m-" and 16 hex digits, for a
// process that runs without a harness session id.
func NewSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return "m-" + hex.EncodeToString(b[:])
}
