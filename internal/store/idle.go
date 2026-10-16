package store

import (
	"context"
	"crypto/tls"

	"github.com/jackc/pgx/v5"
)

// A connection idle in the pool is one Postgres may have ended: a
// restart, pg_terminate_backend or idle_session_timeout ends it with an
// error message (SQLSTATE 57P01, 57P02 or 57P05) and closes its end, and
// the pool learns of that only when the next statement fails. pgxpool
// pings a connection idle for more than a second before it hands it out;
// one used within the last second, as on a busy server, it hands out
// unchecked. So before the pool hands out any connection, the store looks
// at it without a round trip (idleNews): a healthy Postgres sends nothing
// to a connection between statements, so anything waiting to be read, or
// its end closed, means the session is over, and the pool makes another.
//
// That costs a system call an acquire, not a ping's round trip, and
// covers what happened before the acquire: a restart in the middle of a
// statement still fails that statement.

// usable reports whether the pool may hand out conn, which is idle: false
// when Postgres has sent it anything or closed it since its last
// statement.
func usable(_ context.Context, conn *pgx.Conn) (bool, error) {
	if conn.IsClosed() {
		return false, nil
	}
	nc := conn.PgConn().Conn()
	if tc, ok := nc.(*tls.Conn); ok {
		nc = tc.NetConn()
	}
	return !idleNews(nc), nil
}
