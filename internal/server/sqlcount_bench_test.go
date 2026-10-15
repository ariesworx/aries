package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sqlCounter stands between a store and Postgres and counts what the
// store sends: a unix socket that passes every byte on to the server's
// own, and reads the frontend's message headers on the way. It is how
// the benchmarks say how many statements and round trips an op costs,
// which no hook of the store's or the driver's tells.
//
// After the startup message, which has no type, each message is a type
// byte and a length. The store's driver prepares each statement it
// sends with arguments once per connection and keeps it: the first run
// is a Parse, a Describe and a Sync, one round trip, and every run a
// Bind, an Execute and a Sync, another. A statement without arguments,
// such as a transaction's begin or commit, is a simple Query, one round
// trip. The server answers each Sync and each Query.
type sqlCounter struct {
	l      net.Listener
	server string // the server's socket
	wg     sync.WaitGroup

	// queries are statements sent as text (Query), prepares the Parses
	// that prepared one, executes the Executes that ran one, syncs the
	// Syncs that ended a round trip, and other every message else.
	queries, prepares, executes, syncs, other atomic.Int64
	// commits are the queries that are a commit. waited is how long, in
	// nanoseconds, the store's connections waited for the first byte of
	// an answer, over every round trip, and commitWaited over the
	// commits.
	commits, waited, commitWaited atomic.Int64
}

// sqlCount is a reading of a sqlCounter.
type sqlCount struct {
	// Statements are the statements run: those sent as text and those
	// executed after a prepare.
	Statements int64
	// RoundTrips are the messages the server answered: each Query and
	// each Sync.
	RoundTrips int64
	// Prepared are the statements that were prepared first, and Commits
	// the statements that were a commit.
	Prepared, Commits int64
	// Waited is how long the store waited on the server, from each round
	// trip's last byte sent to its answer's first byte received, and
	// CommitWaited the part of it that was a commit's.
	Waited, CommitWaited time.Duration
}

func (a sqlCount) sub(b sqlCount) sqlCount {
	return sqlCount{a.Statements - b.Statements, a.RoundTrips - b.RoundTrips, a.Prepared - b.Prepared, a.Commits - b.Commits,
		a.Waited - b.Waited, a.CommitWaited - b.CommitWaited}
}

func (a sqlCount) add(b sqlCount) sqlCount {
	return sqlCount{a.Statements + b.Statements, a.RoundTrips + b.RoundTrips, a.Prepared + b.Prepared, a.Commits + b.Commits,
		a.Waited + b.Waited, a.CommitWaited + b.CommitWaited}
}

// read returns the counts so far.
func (c *sqlCounter) read() sqlCount {
	q, e, s := c.queries.Load(), c.executes.Load(), c.syncs.Load()
	return sqlCount{Statements: q + e, RoundTrips: q + s, Prepared: c.prepares.Load(), Commits: c.commits.Load(),
		Waited: time.Duration(c.waited.Load()), CommitWaited: time.Duration(c.commitWaited.Load())}
}

// countSQL returns a DSN that reaches dsn's database through a new
// sqlCounter, and the counter. The counter stops, and its socket is
// removed, when the benchmark ends.
func countSQL(tb testing.TB, dsn string) (string, *sqlCounter) {
	tb.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		tb.Fatal(err)
	}
	q := u.Query()
	sockDir, port := q.Get("host"), q.Get("port")
	if !filepath.IsAbs(sockDir) {
		tb.Fatalf("the test server's DSN is on host %q, and the counter passes on a unix socket only", sockDir)
	}
	// A unix socket's path is short; the test's own directory may not be.
	dir, err := os.MkdirTemp("", "ariessql")
	if err != nil {
		tb.Fatal(err)
	}
	c := &sqlCounter{server: filepath.Join(sockDir, ".s.PGSQL."+port)}
	if c.l, err = net.Listen("unix", filepath.Join(dir, ".s.PGSQL."+port)); err != nil {
		_ = os.RemoveAll(dir)
		tb.Fatal(err)
	}
	var open sync.Map // the connections, to close when the counter stops
	c.wg.Go(func() {
		for {
			client, err := c.l.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("unix", c.server)
			if err != nil {
				_ = client.Close()
				continue
			}
			open.Store(client, server)
			asked := &sqlAsked{}
			c.wg.Go(func() {
				c.answers(client, server, asked)
				_ = client.Close()
			})
			c.wg.Go(func() {
				c.pass(server, client, asked)
				_ = server.Close()
				_ = client.Close()
				open.Delete(client)
			})
		}
	})
	tb.Cleanup(func() {
		_ = c.l.Close()
		open.Range(func(client, server any) bool {
			_ = client.(net.Conn).Close()
			_ = server.(net.Conn).Close()
			return true
		})
		c.wg.Wait()
		_ = os.RemoveAll(dir)
	})
	q.Set("host", dir)
	u.RawQuery = q.Encode()
	return u.String(), c
}

// The frontend's message types, as far as the counter tells them apart,
// and the code of the startup message.
const (
	msgQuery   = 'Q'
	msgParse   = 'P'
	msgExecute = 'E'
	msgSync    = 'S'
	// startupCode is protocol 3.0, which the startup message carries
	// where a typed message has its type.
	startupCode = 196608
)

// sqlAsked is the round trip one connection waits on the server for:
// when it was sent, in Unix nanoseconds, or 0 once its answer has
// begun, and whether it is a commit.
type sqlAsked struct {
	at     atomic.Int64
	commit atomic.Bool
}

// answers copies the server's answers to the client, unread, and notes
// how long each took to begin.
func (c *sqlCounter) answers(client io.Writer, server io.Reader, asked *sqlAsked) {
	buf := make([]byte, 64<<10)
	for {
		n, err := server.Read(buf)
		if n > 0 {
			if at := asked.at.Swap(0); at != 0 {
				took := time.Now().UnixNano() - at
				c.waited.Add(took)
				if asked.commit.Load() {
					c.commitWaited.Add(took)
				}
			}
			if _, werr := client.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pass copies the client's messages to the server, counting them, until
// either side ends. It allocates once, so that a benchmark's allocation
// counts stay the daemon's.
func (c *sqlCounter) pass(server io.Writer, client io.Reader, asked *sqlAsked) {
	buf := make([]byte, 5, 64<<10)
	started := false
	for {
		// Before the startup message, a message is a length and a code;
		// after it, a type, then the length.
		head := 5
		if !started {
			head = 4
		}
		buf = buf[:head]
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint32(buf[head-4:])) - 4
		if n < 0 {
			return
		}
		if cap(buf) < head+n {
			grown := make([]byte, head, head+n)
			copy(grown, buf)
			buf = grown
		}
		buf = buf[:head+n]
		if _, err := io.ReadFull(client, buf[head:]); err != nil {
			return
		}
		switch {
		case !started:
			started = n >= 4 && binary.BigEndian.Uint32(buf[4:8]) == startupCode
		case buf[0] == msgQuery:
			c.queries.Add(1)
			commit := bytes.EqualFold(bytes.TrimRight(buf[5:], "\x00 ;"), []byte("commit"))
			if commit {
				c.commits.Add(1)
			}
			asked.commit.Store(commit)
			asked.at.Store(time.Now().UnixNano())
		case buf[0] == msgSync:
			c.syncs.Add(1)
			asked.commit.Store(false)
			asked.at.Store(time.Now().UnixNano())
		case buf[0] == msgParse:
			c.prepares.Add(1)
		case buf[0] == msgExecute:
			c.executes.Add(1)
		default:
			c.other.Add(1)
		}
		if _, err := server.Write(buf); err != nil {
			return
		}
	}
}
