# Onboarding a developer, an agent or CI

Three parts: the person's key, the repository's `.aries.yaml`, and
their agents (docs/install.md#install-aries, docs/cli.md#connect-a-repository,
docs/agents.md#set-up-an-agent). You do the admin's half. The person
does their own half on their own machine; you may be on theirs only when
they are the operator.

## The key: they make it and keep it

- **The person makes the key pair** on their own machine and sends the
  admin the `.pub` file alone (docs/cli.md#your-key-is-your-identity).
  A default name such as `~/.ssh/id_ed25519` needs no configuration; a
  key on a hardware token or behind a passphrase is used through their
  ssh-agent, which they load themselves.
- **You never see a private key.** You receive a path to a `.pub` file.
  Check it before anything reads it: `ssh-keygen -lf FILE.pub`, and
  `grep -c 'PRIVATE KEY' FILE.pub`, which must print 0. If either fails,
  stop: ask for the public half and say why. `aries admin` refuses a
  private key as well, before it sends anything.
- **Check the fingerprint with its owner** over a channel other than the
  one the file came by, before you add it.
- **Add it** as references/access.md says: `aries -C REPO admin principal
  add NAME --pubkey FILE.pub --project P` for someone new, or `aries -C REPO
  admin key add NAME FILE.pub` for a further machine of someone known.

## The repository

- **`.aries.yaml`** holds the project's UUID and the server's URL, with
  the site's CA where the system's store lacks it, and with SSH on the
  host and pinned host key; no secret, committed
  (docs/cli.md#connect-a-repository).
  `aries admin project add` prints it for a new project, and the installer
  for the first. Writing it into a repository and committing it are
  changes: show the file and wait for a yes.
- **Behind IAP**, add the `iap` block; each person needs `gcloud`
  signed in and the tunnel role (docs/cli.md#servers-without-a-public-ip-google-cloud-iap).
- **The person checks**, in the repository: `aries whoami` (their
  principal and the key that signed in) and `aries ready`.

## Their agents

`aries setup AGENT` writes, per harness, the MCP config, a pointer in the
agent's instruction file and the session hooks (docs/agents.md#set-up-an-agent).
Run it in the repository, dry first:

1. `aries -C REPO setup claude-code` prints what it would write. Show it.
2. With a yes: `aries -C REPO setup claude-code --write`, or `aries -C REPO
   setup --all --write` for every harness. It is safe to rerun.
3. `aries -C REPO setup claude-code --check` confirms it.
4. Tell the person the step setup prints that is theirs, such as
   approving the project's MCP server when Claude Code asks.

The files it writes are committed with the repository. `--global`
writes the person's own home files instead (docs/agents.md#setting-agents-up-for-every-repository---global):
only on their machine, with their yes.

## An agent or CI of its own

An agent that works on its own, and every CI job, gets a principal and
key of its own, not a person's (docs/agents.md#give-an-agent-its-own-identity,
docs/deploy/team.md#build-it, steps 4 and 6).

- **The key is made where it will be kept,** by a person: on their
  machine for a local agent (`ssh-keygen -t ed25519 -N '' -f
  ~/.ssh/aries_agent`, run by them), or once for CI with the private
  half going straight into the CI's secret store and both files then
  deleted. You run neither command and see neither private file. Never
  name the secret `ARIES_KEY_FILE`.
- **You add the public half** under its own name, such as `ci-bot`:
  `aries -C REPO admin principal add ci-bot --pubkey ci-bot.pub --project P`.
  Make it no admin.
- **The agent names its key** with `ARIES_KEY_FILE`, which holds the
  key file's path, never the key; with `aries setup --global --key-file
  PATH` for a desktop app (docs/agents.md#where-each-harness-gets-the-variable).
- **CI gates** have their own set-up: docs/ci.md#set-it-up.
- **Check** with `aries whoami` as the agent or job: it prints the
  principal and the key's fingerprint, never the key.
