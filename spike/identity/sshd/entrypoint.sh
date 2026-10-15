#!/bin/sh
# Sets the machine's zone from ZONE, then runs sshd in the foreground,
# logging to standard error.
set -eu
zone=${ZONE:-UTC}
ln -sf "/usr/share/zoneinfo/$zone" /etc/localtime
echo "$zone" > /etc/timezone
export TZ="$zone"
exec /usr/sbin/sshd -D -e
