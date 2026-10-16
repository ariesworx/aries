#!/usr/bin/env bash
# Run once, by the official image's entrypoint, as the postgres user,
# when the database cluster is made on a new volume: docs/server.md's
# step 2, as an administrator runs it for a Postgres ariesd reaches over
# TCP ("Managed services"), so ariesd's role has a password. The
# password is generated here into a 0600 file on the dbsecret volume,
# which the server's entrypoint copies into the password file its DSN
# names. It travels to Postgres on stdin, never in an argument, and is
# never printed.
set -euo pipefail
umask 077

secret=/aries-db/password
pw=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
printf '%s\n' "$pw" >"$secret"
{
  printf "CREATE ROLE aries LOGIN PASSWORD '%s';\n" "$pw"
  echo "CREATE DATABASE aries OWNER aries TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LOCALE 'C';"
  echo "REVOKE ALL ON DATABASE aries FROM PUBLIC;"
} | psql -X -q -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres
echo "aries-docker: made the role aries, with a generated password, and its database aries"
