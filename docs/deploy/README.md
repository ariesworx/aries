# Deployment examples

Three worked examples, from one laptop to a company. All three run the same
server, set up as in [Running a server](../server.md). What changes is
where it runs, how people and agents reach it, and how it is protected and
backed up.

| | [Local](local.md) | [Team](team.md) | [Enterprise](enterprise.md) |
|---|---|---|---|
| **For** | One developer and their agents | A small team, with agents on laptops, in cloud sandboxes and in CI | A company that keeps the tracker off the internet |
| **Server** | Your own Linux machine, WSL2 or a Linux VM | A small cloud VM | A private cloud VM with a separate data disk |
| **Clients connect** | Over SSH to `localhost` | Over SSH across the internet, with the host key pinned | Through Google Cloud IAP, with no public address |
| **Who may connect** | You | One key per person and per automated agent | Keys per person, and an IAM group for the network path |
| **Backups** | Daily dumps to another disk | Daily dumps, carried off the machine by daily disk snapshots | Snapshots, and dumps to a bucket nothing on the VM can delete |
| **Monitoring** | None needed | The provider's VM metrics and ariesd's log | Alerts on the VM, its disk and ariesd's errors, and a budget |
| **Cost a month** | $0 | About $19 | About $65 to $114 |

Every example's server runs Ubuntu 24.04 LTS; 26.04 LTS is supported
too ([What a server needs](../server.md#what-a-server-needs)); systems that
run SELinux are not supported yet.

Costs are Google Cloud list prices for us-central1, on demand and before
tax, read on 7 October 2026. Each example links the pricing pages; check
them before you rely on a figure.

To try Aries, or test a branch, on any machine with Docker, use the
[Docker test server](docker.md). It is for testing, not a deployment.

## What every deployment shares

- **One server holds one project** as these examples set it up. You can
  switch a server to hold many, each in its own schema, with access by
  membership and admins who add projects and people from anywhere
  ([Serving many projects](../server.md#serving-many-projects)). Read
  what that section says is not yet checked on real machines before you
  rely on it.
- **Size the machine by its connections more than its data.** Each
  connection costs about 5 MiB, most of it sshd and the bridge outside
  ariesd, so 64 take about 320 MiB. Postgres took about 50 MiB idle and
  about 2.5 MiB more for each project in use, through ariesd's one pool
  of connections, and gives back what a project held once the pool
  retires its connections ([Sizing Postgres](../server.md#sizing-postgres)
  has the measurements, which are a development machine's). 2 GB suits
  a team with a few projects. `aries admin status` shows ariesd's memory
  and Postgres's own figures
  ([Watching a server](../server.md#watching-a-server)).
- **No high availability.** While the server is down, agents keep working
  on code but cannot claim or finish issues. Recovery is a restart, or a
  [restore](../server.md#backups-and-restore) that loses what changed
  since the last backup. A standby Postgres, or a managed one, makes the
  database survive a machine; ariesd is still one server
  ([Standby and failover](../server.md#standby-and-failover)).
- **Keys are identity.** There is no single sign-on: you manage key lines
  like any other access list ([Principals](../server.md#principals)).
- **Pre-1.0.** Aries is not ready for production use yet; see the status
  in the [README](../../README.md).

## Moving up

A deployment's data moves with one dump: [back up](../server.md#backups-and-restore)
the old server, restore on the new one, and keep its `project` setting.
Keep the host key too, or change it in every `.aries.yaml` as
[Rotating the host key](../server.md#rotating-the-host-key) has it, so
that no client is refused while the files catch up.
