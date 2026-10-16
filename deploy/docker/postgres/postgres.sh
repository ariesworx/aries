#!/usr/bin/env bash
# The entrypoint of the Docker test server's Postgres (compose.yaml):
# the official image's own, given what it needs and nothing a person
# keeps. The image refuses to start without a superuser password; this
# generates one on the first start into a 0600 file on Postgres's volume,
# which nothing prints or uses again, as docs/server.md has no use for
# one either. It also makes the directory on the dbsecret volume where
# init/aries.sh keeps the password of ariesd's role, which only the
# postgres user can read and the server's container reads as root.
set -euo pipefail

# Only the file is made with umask 077: the image's entrypoint makes
# the data directory's parents, which the postgres user must traverse.
superuser=/var/lib/postgresql/superuser.pass
if [[ ! -s $superuser ]]; then
  (umask 077 && od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$superuser")
  chown postgres:postgres "$superuser"
fi
export POSTGRES_PASSWORD_FILE=$superuser
install -d -o postgres -g postgres -m 0700 /aries-db

exec docker-entrypoint.sh "$@"
