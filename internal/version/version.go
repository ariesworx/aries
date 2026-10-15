// Package version holds the build version of both binaries, and the
// Postgres release they are tested with.
//
// [Version] is what aries and ariesd print, send in the handshake and
// compare with releases. [Postgres] is the pin that tests keep equal to
// CI's Postgres, to scripts/install-postgres.sh and to the postgres on
// PATH.
package version

// Version is set at build time:
//
//	go build -ldflags "-X github.com/ariesworx/aries/internal/version.Version=v0.1.0" ./cmd/...
//
// A build without it reports "dev", which never triggers version warnings.
var Version = "dev"

// Postgres is the Postgres release this Aries release is tested with: the
// newest of its major version ([PostgresMajor]) when it was pinned.
// scripts/install-postgres.sh builds it from the source archive whose
// SHA-256 postgres.sha256 pins, and the release notes name it. CI installs
// the newest release of the same major version from the PostgreSQL
// project's apt repository (POSTGRES_MAJOR in .github/workflows/ci.yml; a
// test keeps the two equal), so the tests hold the major version to the
// pin and not the minor.
const Postgres = "18.6"

// PostgresMajor is [Postgres]'s major version.
const PostgresMajor = "18"

// PostgresMin is the oldest major version of Postgres that ariesd opens a
// database on (decision 81); the store refuses an older server.
const PostgresMin = 16
