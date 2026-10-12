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
| `starfixd` | The binary's path, `/opt/starfix/bin/starfixd`, which `/usr/local/bin/starfixd` links to: AppArmor resolves the link | `starfixd stdio`, the bridge sshd runs for every connection |
| `starfixd-serve` | The unit's `AppArmorProfile=starfixd-serve` | `starfixd serve`, the daemon |
| `starfixd-admin` | The operator: `aa-exec -p starfixd-admin -- starfixd …` | `control init`, `import-bd`, `export-bd`, `upgrade` |

- **The path carries the smallest profile**, because sshd runs the
  bridge with no way to ask for another, and outside the systemd unit:
  the unit's `ProtectSystem` and the rest do not reach it. The bridge
  may read the config file and connect to the daemon's socket.
- **The daemon gets its own**, because only it reaches Dolt and writes
  the keys file. With one profile for both, the bridge could do both.
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
  write and lock; its private `/tmp`; a TCP listener; and what the Go
  runtime reads. The same path is also the operator's tool (`dolt sql`,
  `dolt dump`, `dolt gc`), which reads the data directory it is run in
  and writes dumps to `/var/backups/starfix`, so it would need the same
  split as starfixd's, or a profile the unit alone asks for. None of
  this was written or run.

## Checking a change

```sh
apparmor_parser -Q -K -I deploy/apparmor deploy/apparmor/starfixd
```

checks the syntax without loading anything, and needs no root; CI runs
it on its Ubuntu runner. It does not say whether starfixd still works
under the profiles. A change to what starfixd reads, writes, connects
to or runs needs the profiles changed with it, and a run on a machine
with AppArmor.
