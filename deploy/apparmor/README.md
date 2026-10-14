# AppArmor profiles for starfixd

Optional hardening for a starfix server on Ubuntu, whose mandatory
access control is AppArmor. The server guide says how to install, use
and remove them:
[Confine starfixd with AppArmor](../../docs/server.md#confine-starfixd-with-apparmor).

| File | Installed as | Holds |
|---|---|---|
| `starfixd` | `/etc/apparmor.d/starfixd` | Three profiles: `starfixd`, `starfixd-serve` and `starfixd-admin` |
| `tunables/starfixd` | `/etc/apparmor.d/tunables/starfixd` | The paths the profiles name, each in one place, for a server that moved one |

## Why three profiles

The one binary is the daemon, sshd's forced command and the operator's
tool, and AppArmor chooses a profile by a program's path, never by its
arguments.

| Profile | Chosen by | For |
|---|---|---|
| `starfixd` | The binary's path, `/opt/starfix/bin/starfixd`, which `/usr/local/bin/starfixd` links to: AppArmor resolves the link | `starfixd stdio`, the bridge sshd runs for every connection, and `starfixd status` |
| `starfixd-serve` | The unit's `AppArmorProfile=starfixd-serve` | `starfixd serve`, the daemon |
| `starfixd-admin` | The operator: `aa-exec -p starfixd-admin -- starfixd …` | `control init`, `import-bd`, `export-bd`, `upgrade` |

- **The path carries the smallest profile**, because sshd runs the
  bridge with no way to ask for another, and outside the systemd unit:
  the unit's `ProtectSystem` and the rest do not reach it. The bridge
  may read the config file and connect to the daemon's socket.
- **The daemon gets its own**, because only it reaches Dolt and writes
  the keys file. With one profile for both, the bridge could do both.
  It is also what listens on the loopback port for metrics and health,
  posts metrics to a collector, and writes to systemd's notify socket
  ([Observability](../../docs/server.md#observability)).
- **`starfixd status` runs under the bridge's**, by the path alone: it
  reads the config file and connects to the daemon's socket, which is
  all the bridge may do, so it needs no rule and no `aa-exec`. It is run
  as the `starfix` user (`sudo -u starfix -H starfixd status`). root
  cannot run it directly: the config file is the `starfix` user's, mode
  0600, and the bridge's profile gives root no capability to read it.
- **The operator's commands get the widest**, because `upgrade` reaches
  the network, replaces the binary and runs `sudo`, which nothing that
  listens should be able to do. A command run without `aa-exec` gets the
  bridge's profile and is refused.
- No profile may change to another, and neither the bridge nor the
  daemon may run a program.

Each profile ends with explicit `audit deny` rules for what starfixd
must never do: write its config file or `authorized_keys`, write its
binary (but for the operator's profile), read under `/home` or `/root`,
trace another process, or mount. The bridge and the daemon are denied
every capability; the operator's profile has one, so that root can read
the config file for `upgrade --check`. Unix permissions allow the first
three, since the `starfix` user owns those files.

## How they were written and tested

On 12 October 2026, on Ubuntu 24.04.5 (kernel 6.8.0-142, AppArmor
4.0.1, systemd 255, OpenSSH 9.6p1), arm64, in a virtual machine set up
by following the server guide. amd64 was not tested.

1. **Stock state.** `aa-status` showed 117 profiles and none for sshd;
   sshd, its children and starfixd ran `unconfined`.
2. **Complain mode, empty profiles**, to see everything starfixd does,
   with the kernel log's rate limit off (`sysctl -w
   kernel.printk_ratelimit=0`; with the default, 135 lines of some
   15,000 were kept). The rules were then written by hand from that
   list and from the code.
3. **Complain mode, the written profiles.** Nothing was logged in
   correct use.
4. **Enforce mode.** 76 checks (issues, a second project, keys from the
   control database, a key removed, a principal disabled, the
   operator's commands, restarts) passed with no line in the kernel
   log, before and after a reboot.
5. **Negative tests**, each refused and logged as `DENIED`: a config
   file at another path; `/etc/shadow` as root; the keys file and the
   socket at other paths; appending to the config file and to
   `authorized_keys`, and running a program, from a shell inside each
   profile; the bridge reaching Dolt or the network; the daemon
   replacing the binary; the operator's profile writing into `.ssh` or
   outside the top of the home directory; one profile changing to
   another.

What complain mode taught:

- **`attach_disconnected` is required.** The daemon binds its socket
  inside the mount namespace systemd gives the unit, so to the bridge
  the socket's path does not start at the root. AppArmor refuses such a
  path even in complain mode (`Failed name lookup - disconnected path`),
  and every `sfx` command answered `starfixd is not running on the
  server`.
- **Go reads the cgroup's CPU limit** at start (`/proc/PID/cgroup`,
  `/proc/PID/mountinfo`, `cpu.max` under `/sys/fs/cgroup`).
- **A unix socket with a path is a file to AppArmor here.** The bridge
  and the daemon needed `rw` on the socket and no rule naming each
  other.
- **Files a shell opens are not checked again.** A terminal, a pipe or
  a redirection opened by the operator's unconfined shell works under
  any profile, so the profiles need no rule for terminals.
- **`upgrade` looks at every directory above the binary** to resolve
  links, which is why the tunables name them.
- **sudo cannot run inside a profile** without that profile holding
  root's capabilities, so the operator's profile runs it unconfined
  (`Ux`): what it may then do is what sudoers allows the `starfix`
  user, in the guide one `systemctl restart`.

## What the telemetry needed

Slice O1 of the observability work was run on the same machine on 12
October 2026, with the three profiles enforced, the unit of the guide
(`Type=notify`, `WatchdogSec=120`), a listener on `127.0.0.1:9464` and
a throwaway OTLP receiver (Python's `http.server`) on the machine.

| What starfixd does | Needs |
|---|---|
| Tells systemd it is ready, stopping and alive | **A new rule**, `@{run}/systemd/notify w,` in `starfixd-serve`. Without it the daemon serves, logs `could not tell systemd the daemon is ready … permission denied`, and the unit stays `activating` until `TimeoutStartSec`, when systemd stops it and starts it again. The kernel log has `operation="sendmsg" … name="/run/systemd/notify"` |
| Listens on a loopback TCP port | Nothing new: `network inet stream` already covered it |
| Posts metrics to a collector at an address, or at a name `/etc/hosts` holds | Nothing new |
| Posts metrics to a collector whose name needs DNS | The two UDP lines in `local/starfixd-serve`, as for a `dsn` that names Dolt's host. Without them the export fails (`lookup … dial udp 127.0.0.53:53: socket: permission denied`), is logged once, and nothing else is affected |
| Posts metrics over HTTPS | `include <abstractions/ssl_certs>` in `local/starfixd-serve`, so that it can read the machine's certificate authorities. Without it the export fails (`x509: failed to load system roots and no roots provided; open /etc/ssl/certs/ca-certificates.crt: permission denied`), and the kernel log has the denied read |
| `starfixd status` | Nothing new, under the bridge's profile |

The 76 checks of the run above passed again with all of this on, across
a cold boot, with no line in the kernel log; the two refusals that are
logged are root, and another user with a config file of her own, running
`starfixd status`.

Reading Dolt's own metrics (`telemetry: dolt_metrics_url:`, slice O4,
run on the same machine the same day) needed nothing new either: it is
an HTTP GET to `127.0.0.1:11228` every 15 seconds, which `network inet
stream` covers, at an address, so no name is resolved. The 76 checks
passed with it on and no line in the kernel log, and with the unit also
held to loopback (`IPAddressDeny=any`, `IPAddressAllow=localhost`) the
reads went on. Dolt, which listens on that port, is not confined.

So metrics sent **straight to a hosted backend** need three lines the
shipped profile leaves out, since the daemon otherwise speaks to no
machine by name and reads no certificate:

```text
# /etc/apparmor.d/local/starfixd-serve
network inet dgram,
network inet6 dgram,
include <abstractions/ssl_certs>
```

then `apparmor_parser -r /etc/apparmor.d/starfixd` and a restart. A
collector on the machine, which the guide gives first, needs none of
them. HTTPS was run against a receiver on the machine with a
certificate from a private authority added to the machine's trust
store, not against a real backend.

## What traces need

Slice O2 of the observability work has the daemon export traces to the
same collector, and name people on them by a stand-in made with a key
it keeps in a file ([Traces](../../docs/server.md#traces)). It was run
on the same machine on 12 October 2026, with the three profiles
enforced, the guide's unit and its drop-in for the key's directory, and
a throwaway OTLP receiver (Python's `http.server`) on the machine.

| What starfixd does | Needs |
|---|---|
| Posts traces to a collector | Nothing new: the rules that carry metrics carry them. A hosted backend needs the same three lines in `local/starfixd-serve` as for metrics; that was not run again for traces |
| Makes the stand-in key once, and reads it at each start | **A new rule**, `owner @{starfixd_principals_key} rw,` in `starfixd-serve`, and the tunable it names, `/var/lib/starfix/telemetry/principals.key`. The rule names the one file, not its directory. Without it the daemon runs as before and its traces say nothing about who: its log has `telemetry: spans say nothing about who …` with `cause: it cannot be made: permission denied`, its status report says the same, and the kernel log has `operation="mknod" … name="/var/lib/starfix/telemetry/principals.key" … requested_mask="c"` |

With the rule loaded, by `apparmor_parser -r` and with no restart, the
daemon made the key at its next look, half a minute later, with mode
0600, and its spans carried stand-ins from then on. In an earlier run
of the same day it read the key again at a restart and made a new one
after the file was deleted. The 76 checks of the runs above passed with
traces on, and nothing was denied.

The bridge's and the operator's profiles have no rule for the file, so
neither can read it: the operator's allows the top of the home
directory, and the key is one level down. Both were tried as the
`starfix` user, who owns the file, and both were refused and logged as
`DENIED`.

## On Ubuntu 26.04

Run on 14 October 2026 on Ubuntu 26.04.1 (kernel 7.0.0-34, AppArmor
5.0.2, systemd 259, OpenSSH 10.2p1, sudo-rs as `sudo`), arm64, in a
fresh virtual machine set up by `install-server.sh --apparmor`. The
profiles parse under AppArmor 5 with their `abi <abi/4.0>`. Two things
differ from 24.04, and the profiles changed for both:

| What differs | What it did | The change |
|---|---|---|
| `kernel.apparmor_restrict_unprivileged_unconfined` is 1 (0 on 24.04). A process that is not root and changes to a profile gets the profile stacked on `unconfined` | The daemon runs as `starfixd-serve//&unconfined (enforce)` (systemd changes profile after `User=starfix`), and the operator's commands as `starfixd-admin//&unconfined` (`aa-exec` as `starfix`). As root, `aa-exec` still gives a plain label, and with the sysctl at 0 so do both; `no_new_privs` and sudo-rs played no part. `abstractions/base` lets a profile signal `@{profile_name}`, which the stacked label is not, so the kernel refused the Go runtime's `SIGURG` to its own threads: `operation="signal" … signal=urg peer="starfixd-serve//&unconfined"`, three times in the daemon's first half minute | `signal peer=@{profile_name}//&unconfined,` in `starfixd-serve` and `starfixd-admin`. A throwaway pair of profiles entered as another user, alike but for that rule, showed it needed and enough: without it a signal to the process's own label was refused and logged, with it allowed |
| `sudo` is sudo-rs: `/usr/bin/sudo` leads to `/usr/lib/cargo/bin/sudo`, or to `/usr/bin/sudo.ws` where the machine chose the original | `upgrade --rollback --restart` under `starfixd-admin` restored the binary and could not restart the unit: `exec` of `/usr/lib/cargo/bin/sudo` denied, since AppArmor names the file a link leads to | `/usr/bin/sudo.ws rUx,` and `/usr/lib/cargo/bin/sudo rUx,` beside `/usr/bin/sudo rUx,`. Then the same command restarted the unit and found it healthy with either `sudo` |

The bridge, attached by its path, is a plain `starfixd (enforce)`: the
stack comes only from a change of profile. A stack allows only what
each of its profiles allows, and under `starfixd-admin//&unconfined` a
write into `.ssh` was still refused. With the changed profiles, the
installer's checks, 300 requests from three clients at a time, the
operator's commands and two cold boots that were checked logged
nothing of starfixd's. The same profiles were run on a fresh 24.04
machine the same day: plain labels, the same checks, no denial.

Ubuntu 26.04 also ships profiles for many of its own programs, which
log refusals of their own (`who` and `systemd-detect-virt` at boot, on
this machine); look for starfixd's with
`journalctl -k -g 'apparmor="DENIED".*profile="starfixd'`.

## What is not covered

- **A real upgrade.** A build from source refuses to upgrade itself, so
  the download, the signature check and the swap to a new binary did
  not run. `upgrade --check` and `--rollback` did, and so did each file
  operation of the swap by itself: creating the staged files, and
  running a staged binary for its version.
- **The keys file.** The daemon must write the file sshd reads keys
  from, and sshd accepts any line in it, so these profiles do not keep
  a daemon that has been taken over from letting a key in.
- **Addresses.** `starfixd-serve` may open TCP connections to any
  address. `apparmor_parser` accepted a rule naming an address and a
  port; whether the kernel enforces it was not tested, and the profiles
  do not rely on it. `IPAddressDeny=any` with `IPAddressAllow=localhost`
  in the unit was run and holds the daemon to loopback.
- **sshd**, which Ubuntu does not confine.
- **Dolt.** A profile for it would be attached to `/usr/local/bin/dolt`
  and need: its config file, read; its data directory, `cfg_dir` and
  the account's own `.dolt` directory under `/var/lib/dolt`, read,
  write and lock; its private `/tmp`; its TCP listeners, for SQL and
  for its metrics; and what the Go runtime reads. The same path is also the operator's tool (`dolt sql`,
  `dolt dump`, `dolt gc`), which reads the data directory it is run in
  and writes dumps to `/var/backups/starfix`, so it would need the same
  split as starfixd's, or a profile the unit alone asks for. None of
  this was written or run.

## Checking a change

```sh
apparmor_parser -Q -K -I deploy/apparmor deploy/apparmor/starfixd
```

checks the syntax without loading anything, and needs no root; CI runs
it on an Ubuntu 24.04 runner (AppArmor 4) and an Ubuntu 26.04 runner
(AppArmor 5). It does not say whether starfixd still works
under the profiles. A change to what starfixd reads, writes, connects
to or runs needs the profiles changed with it, and a run on a machine
with AppArmor.
