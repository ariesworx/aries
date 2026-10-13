package server

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// sqlCounter stands between a store and Dolt and counts what the store
// sends: a unix socket that passes every byte on to Dolt's own, and
// reads the MySQL packet headers on the way. It is how the benchmarks
// say how many statements and round trips an op costs, which no hook
// of the store's or the driver's tells.
//
// A command is a client packet with sequence number 0; its first byte
// names it. The store's driver prepares every statement that has
// arguments (the store turns InterpolateParams off), so such a
// statement is three commands and two round trips: prepare, execute,
// and a close that has no answer.
type sqlCounter struct {
	l    net.Listener
	dolt string // Dolt's socket
	wg   sync.WaitGroup

	// queries are statements sent as text (COM_QUERY), prepares and
	// executes the two halves of one sent with arguments, closes the
	// unanswered close of a prepared statement, and other every command
	// else, a ping or a reset among them.
	queries, prepares, executes, closes, other atomic.Int64
	// commits are the queries that are a COMMIT. waited is how long, in
	// nanoseconds, the store's connections waited for the first byte of
	// an answer, over every command, and commitWaited over the commits.
	commits, waited, commitWaited atomic.Int64
}

// sqlCount is a reading of a sqlCounter.
type sqlCount struct {
	// Statements are the statements run: those sent as text and those
	// executed after a prepare.
	Statements int64
	// RoundTrips are the commands Dolt answered: every one but a close.
	RoundTrips int64
	// Prepared are the statements that were prepared first, and Commits
	// the statements that were a COMMIT.
	Prepared, Commits int64
	// Waited is how long the store waited on Dolt, from each command's
	// last byte sent to its answer's first byte received, and
	// CommitWaited the part of it that was a COMMIT's.
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
	q, p, e, o := c.queries.Load(), c.prepares.Load(), c.executes.Load(), c.other.Load()
	return sqlCount{Statements: q + e, RoundTrips: q + p + e + o, Prepared: p, Commits: c.commits.Load(),
		Waited: time.Duration(c.waited.Load()), CommitWaited: time.Duration(c.commitWaited.Load())}
}

// countSQL returns a DSN that reaches dsn's database through a new
// sqlCounter, and the counter. The counter stops, and its socket is
// removed, when the benchmark ends.
func countSQL(tb testing.TB, dsn string) (string, *sqlCounter) {
	tb.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		tb.Fatal(err)
	}
	if cfg.Net != "unix" {
		tb.Fatalf("the test Dolt's DSN is on %q, and the counter passes on a unix socket only", cfg.Net)
	}
	// A unix socket's path is short; the test's own directory may not be.
	dir, err := os.MkdirTemp("", "sfxsql")
	if err != nil {
		tb.Fatal(err)
	}
	c := &sqlCounter{dolt: cfg.Addr}
	if c.l, err = net.Listen("unix", filepath.Join(dir, "s")); err != nil {
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
			server, err := net.Dial("unix", c.dolt)
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
	cfg.Addr = c.l.Addr().String()
	return cfg.FormatDSN(), c
}

// MySQL's command bytes, as far as the counter tells them apart.
const (
	comQuery       = 0x03
	comStmtPrepare = 0x16
	comStmtExecute = 0x17
	comStmtClose   = 0x19
)

// sqlAsked is the command one connection waits on Dolt for: when it was
// sent, in Unix nanoseconds, or 0 once its answer has begun, and
// whether it is a COMMIT.
type sqlAsked struct {
	at     atomic.Int64
	commit atomic.Bool
}

// answers copies Dolt's answers to the client, unread, and notes how
// long each took to begin.
func (c *sqlCounter) answers(client io.Writer, dolt io.Reader, asked *sqlAsked) {
	buf := make([]byte, 64<<10)
	for {
		n, err := dolt.Read(buf)
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

// pass copies the client's packets to Dolt, counting the commands, until
// either side ends. It allocates once, so that a benchmark's allocation
// counts stay the daemon's.
func (c *sqlCounter) pass(dolt io.Writer, client io.Reader, asked *sqlAsked) {
	buf := make([]byte, 4, 64<<10)
	for {
		buf = buf[:4]
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		n := int(binary.LittleEndian.Uint32([]byte{buf[0], buf[1], buf[2], 0}))
		seq := buf[3]
		if cap(buf) < 4+n {
			grown := make([]byte, 4, 4+n)
			copy(grown, buf)
			buf = grown
		}
		buf = buf[:4+n]
		if _, err := io.ReadFull(client, buf[4:]); err != nil {
			return
		}
		if seq == 0 && n > 0 {
			commit := buf[4] == comQuery && string(buf[5:]) == "COMMIT"
			if commit {
				c.commits.Add(1)
			}
			if buf[4] != comStmtClose { // a close has no answer to wait for
				asked.commit.Store(commit)
				asked.at.Store(time.Now().UnixNano())
			}
			switch buf[4] {
			case comQuery:
				c.queries.Add(1)
			case comStmtPrepare:
				c.prepares.Add(1)
			case comStmtExecute:
				c.executes.Add(1)
			case comStmtClose:
				c.closes.Add(1)
			default:
				c.other.Add(1)
			}
		}
		if _, err := dolt.Write(buf); err != nil {
			return
		}
	}
}
