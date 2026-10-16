# Provisioning the machine

Before install: a virtual machine, its firewall, and the path people
take to it. This is vendor-neutral. The guides work two examples on
Google Cloud, with every command: docs/deploy/team.md#build-it (a public
address, port 22 rate-limited) and docs/deploy/enterprise.md#build-it
(no public address, through IAP). docs/deploy/README.md#what-every-deployment-shares
says what any deployment needs.

Cloud commands run on the workstation with the operator's own cloud
CLI and credentials. You never read a credentials file or print a
token. Creating resources costs money: show the command, the resource,
its size and region, and the guide's cost figure where it has one
(docs/deploy/README.md), and wait for a yes.

## Decide first, with the operator

| Choice | Options | Guide |
|---|---|---|
| Who connects, from where | Laptops only; also cloud sandboxes and CI | docs/deploy/README.md |
| The path to sshd | A public address with port 22 rate-limited; Google Cloud IAP (the one private path `aries` supports natively); a VPN such as WireGuard, with `host` the private address | docs/deploy/enterprise.md#on-other-clouds, docs/cli.md#servers-without-a-public-ip-google-cloud-iap |
| One project or many | The installer sets up a server of many projects | docs/server.md#serving-many-projects |
| Size | 2 GB for a team with a few projects; more for many agents, each connection costing about 5 MiB | docs/server.md#sizing-postgres |
| Data disk | A separate disk for the database's data, so the boot disk can be rebuilt | docs/deploy/enterprise.md#choices-this-example-makes |
| Backups | Disk snapshots, and dumps carried off the machine | docs/server.md#backups-and-restore |

## The machine

- **Ubuntu 24.04 LTS or 26.04 LTS**, amd64 or arm64, with systemd and
  OpenSSH (docs/server.md#what-a-server-needs). Not a system that runs
  SELinux. The guide was run on arm64 virtual machines, not on a cloud
  provider's image; say so to the operator.
- **No other service on it.** ariesd, sshd and the database only.
- **A time service** the machine reaches without the internet where the
  provider has one (docs/server.md#keep-the-clock-synchronized).
- **The provider's login service** (OS Login, EC2 Instance Connect,
  Entra ID login) may stay: the installer's sshd drop-in keeps it away
  from the `aries` account (docs/server.md#6-let-the-first-principal-in).
  The operator's own administrative login is theirs to set up, with
  `sudo`.

## The firewall

- **Inbound: SSH only**, on the one port clients use. Nothing else: the
  database and ariesd's listener (9464) answer on loopback alone and
  must stay so. A managed Postgres is reached on the provider's private
  network, never the internet, and with TLS (references/database.md,
  docs/server.md#managed-services).
- **Behind IAP**, admit only IAP's range, `35.235.240.0/20`, on port 22
  (docs/deploy/enterprise.md#build-it, step 1).
- **On a public address**, rate-limit new connections on the machine
  (`ufw limit 22/tcp`) after install, unless everyone arrives through a
  few shared addresses (docs/server.md#6-let-the-first-principal-in).
- **Outbound**: HTTPS to GitHub (or the release mirror) for releases
  and to the OS's archive. Without it, carry releases in
  (docs/server.md#upgrades, "A server with no way out").

## The host key

Every `.aries.yaml` pins sshd's ED25519 host key, and `aries` trusts
none on first use (docs/cli.md#the-host-key-is-pinned). Get the
fingerprint from the provider, not across the network: the serial
console's output, or a guest attribute written at boot
(docs/deploy/enterprise.md#build-it, step 5). The operator checks it
before the first `ssh`.

## Other clouds

The guides have worked examples for Google Cloud only. On another
provider, do the same: a VM as above, a security group or firewall that
admits SSH only (from the VPN's or bastion's range, for a private path),
snapshots of the data disk, and the host key from the provider's
console. Name each resource after the operator's own convention, and
say plainly that the guide has not run that provider.

## Done when

`ssh -o BatchMode=yes HOST true` works with a host key the operator
verified, `cat /etc/os-release` says a supported Ubuntu, and the
firewall admits nothing but SSH. Then install (references/install.md).
