-- A project's schema, in Postgres's dialect: issues and what hangs off
-- them, claims, agents, the inbox, idempotency, memory, usage, prices,
-- hours and plans, conflicts, gates, locks, the event log and settings.
-- This one migration is the schema as it stood when the store moved from
-- Dolt to Postgres (decision 79); later changes are later migrations, and
-- none of them is ever edited once it has merged.
--
-- Mutable rows carry rev, which each write that changes the row raises
-- by one: an update that names the rev it read (WHERE rev = $n) is a
-- compare-and-swap, since a second writer of the row waits for the
-- first's row lock and then matches no row. Times are TIMESTAMP(6),
-- without a time zone, always in UTC. A column that compares byte for
-- byte, whatever the database's collation, says COLLATE "C".
-- Statements end with a semicolon at the end of a line.

-- Issues. account is a client's engagement code name or an internal
-- department; NULL inherits from the parent chain, then the server's
-- account: setting. Similar closed issues scan the most recently closed
-- issues' titles (issues_closed).
CREATE TABLE issues (
  id           VARCHAR(64)   NOT NULL,
  parent_id    VARCHAR(64)   NULL,
  title        VARCHAR(500)  NOT NULL,
  body         TEXT          NOT NULL,
  design       TEXT          NOT NULL,
  acceptance   TEXT          NOT NULL,
  notes        TEXT          NOT NULL,
  status       VARCHAR(32)   NOT NULL,
  priority     SMALLINT      NOT NULL,
  type         VARCHAR(32)   NOT NULL,
  assignee     VARCHAR(255)  NULL,
  owner        VARCHAR(255)  NULL,
  due_at       TIMESTAMP(6)  NULL,
  defer_until  TIMESTAMP(6)  NULL,
  ephemeral    BOOLEAN       NOT NULL DEFAULT FALSE,
  expires_at   TIMESTAMP(6)  NULL,
  pinned       BOOLEAN       NOT NULL DEFAULT FALSE,
  template     BOOLEAN       NOT NULL DEFAULT FALSE,
  metadata     JSON          NULL,
  close_reason VARCHAR(2000) NULL,
  created_by   VARCHAR(255)  NOT NULL,
  created_at   TIMESTAMP(6)  NOT NULL,
  updated_at   TIMESTAMP(6)  NOT NULL,
  closed_at    TIMESTAMP(6)  NULL,
  account      VARCHAR(64)   NULL,
  rev          BIGINT        NOT NULL,
  PRIMARY KEY (id)
);
CREATE INDEX issues_parent ON issues (parent_id);
CREATE INDEX issues_ready ON issues (status, priority, created_at);
CREATE INDEX issues_created ON issues (created_at, id);
CREATE INDEX issues_closed ON issues (status, closed_at);

CREATE TABLE deps (
  from_id    VARCHAR(64)  NOT NULL,
  to_id      VARCHAR(64)  NOT NULL,
  type       VARCHAR(32)  NOT NULL,
  metadata   JSON         NULL,
  created_by VARCHAR(255) NOT NULL,
  created_at TIMESTAMP(6) NOT NULL,
  rev        BIGINT       NOT NULL,
  PRIMARY KEY (from_id, to_id, type)
);
CREATE INDEX deps_to ON deps (to_id);

CREATE TABLE labels (
  issue_id   VARCHAR(64)  NOT NULL,
  label      VARCHAR(64)  NOT NULL,
  created_at TIMESTAMP(6) NOT NULL,
  PRIMARY KEY (issue_id, label)
);
CREATE INDEX labels_label ON labels (label);

-- Comments. kind is '' for a plain comment and 'handoff' for a handoff
-- note, which start returns.
CREATE TABLE comments (
  id         VARCHAR(32)  NOT NULL,
  issue_id   VARCHAR(64)  NOT NULL,
  author     VARCHAR(255) NOT NULL,
  session    VARCHAR(255) NOT NULL,
  body       TEXT         NOT NULL,
  created_at TIMESTAMP(6) NOT NULL,
  kind       VARCHAR(16)  NOT NULL DEFAULT '',
  PRIMARY KEY (id)
);
CREATE INDEX comments_issue ON comments (issue_id, created_at);

-- The event log, the truth that ready, blocked and the digest derive
-- from. seq is gapless: the store's one writer gives each event the next.
--
-- idem_key is a client's key on the last event of a create-type
-- operation (create, finish, comment, handoff), so a retried request
-- returns the first one's result instead of writing again. A key belongs
-- to the principal that sent it, so the unique index is on (principal,
-- idem_key); NULL keys never collide. idem_args is a SHA-256 of the
-- request, to refuse a key reused for a different one; idem_result is
-- the result to replay.
--
-- rev is the issue's rev after the event, on the one event of each write
-- that changed the issue's row, and NULL on every other: conflicts
-- (design §18.7) compare a replayed update against the state at the rev
-- it was based on, which these give. The digest ranges over at
-- (events_at); attribution finds the issues a session took from its
-- claim.take events (events_actor_op).
CREATE TABLE events (
  seq          BIGINT       NOT NULL,
  at           TIMESTAMP(6) NOT NULL,
  principal    VARCHAR(255) NOT NULL,
  session      VARCHAR(255) NOT NULL,
  machine      VARCHAR(255) NOT NULL,
  op           VARCHAR(64)  NOT NULL,
  target       VARCHAR(64)  NOT NULL,
  before_state JSON         NULL,
  after_state  JSON         NULL,
  idem_key     VARCHAR(128) NULL,
  idem_args    CHAR(64)     NULL,
  idem_result  JSON         NULL,
  rev          BIGINT       NULL,
  PRIMARY KEY (seq)
);
CREATE UNIQUE INDEX events_idem ON events (principal, idem_key);
CREATE INDEX events_target ON events (target, seq);
CREATE INDEX events_at ON events (at);
CREATE INDEX events_actor_op ON events (op, principal, session);

CREATE TABLE settings (
  name       VARCHAR(128) NOT NULL,
  value      JSON         NOT NULL,
  source     VARCHAR(32)  NOT NULL,
  updated_at TIMESTAMP(6) NOT NULL,
  rev        BIGINT       NOT NULL,
  PRIMARY KEY (name)
);

-- Claims: who holds an issue, until when, and the fencing epoch. One row
-- per issue that was ever taken; a released or expired claim keeps its row
-- with no holder, so the epoch only ever increases.
CREATE TABLE claims (
  issue_id   VARCHAR(64)  NOT NULL,
  principal  VARCHAR(255) NULL,
  session    VARCHAR(255) NULL,
  machine    VARCHAR(255) NULL,
  epoch      BIGINT       NOT NULL,
  claimed_at TIMESTAMP(6) NULL,
  expires_at TIMESTAMP(6) NULL,
  rev        BIGINT       NOT NULL,
  PRIMARY KEY (issue_id)
);
CREATE INDEX claims_expires ON claims (expires_at);
CREATE INDEX claims_holder ON claims (principal, session);

-- Agents: the live registry behind `who`. One row per (principal,
-- session), touched when a session connects and while it renews; who
-- lists the rows seen recently. harness is empty when the client did not
-- say which agent it runs under.
CREATE TABLE agents (
  principal VARCHAR(255) NOT NULL,
  session   VARCHAR(255) NOT NULL,
  machine   VARCHAR(255) NOT NULL,
  harness   VARCHAR(32)  NOT NULL,
  started   TIMESTAMP(6) NOT NULL,
  last_seen TIMESTAMP(6) NOT NULL,
  rev       BIGINT       NOT NULL,
  PRIMARY KEY (principal, session)
);
CREATE INDEX agents_last_seen ON agents (last_seen);

-- Inbox: items for a principal, made in the same transaction as the
-- change that causes them (a lost claim, an assignment, a mention, a
-- handoff). to_session is NULL for an item any session of the principal
-- may read, or names the one session it concerns. id rises with each
-- item, so the newest sort first; read_at is set when it is acked.
CREATE TABLE inbox (
  id             BIGINT       NOT NULL,
  to_principal   VARCHAR(255) NOT NULL,
  to_session     VARCHAR(255) NULL,
  kind           VARCHAR(32)  NOT NULL,
  issue_id       VARCHAR(64)  NULL,
  body           VARCHAR(512) NOT NULL,
  from_principal VARCHAR(255) NOT NULL,
  at             TIMESTAMP(6) NOT NULL,
  read_at        TIMESTAMP(6) NULL,
  PRIMARY KEY (id)
);
CREATE INDEX inbox_to ON inbox (to_principal, read_at);

-- Handoffs: the structured fields of a handoff note. The note itself stays
-- a comment of kind 'handoff', so comments, history and a bd export are
-- unchanged; this row, keyed by that comment, holds what the next person
-- needs in fields: the state of the work, the next step, the branch and
-- worktree it is on, and whom it is handed to. A note with none of them
-- has no row. Empty strings mean "not given".
CREATE TABLE handoffs (
  comment_id   VARCHAR(32)   NOT NULL,
  issue_id     VARCHAR(64)   NOT NULL,
  state        VARCHAR(16)   NOT NULL,
  next_step    VARCHAR(500)  NOT NULL,
  branch       VARCHAR(255)  NOT NULL,
  worktree     VARCHAR(1024) NOT NULL,
  to_principal VARCHAR(255)  NOT NULL,
  PRIMARY KEY (comment_id)
);
CREATE INDEX handoffs_issue ON handoffs (issue_id);

-- Acceptance checklist: an issue's items are parsed from its acceptance
-- text (a Markdown list, or the whole text as one item), so they never go
-- stale when the text is edited or imported. This table holds only what
-- people did to them: one row per issue and item text (item_key is the
-- SHA-256 of the item's text), so a tick follows its item when the list
-- is reordered. state is 'ticked', 'waived' (with a reason) or 'open' (an
-- item ticked in the text, unticked since). n is the item's position when
-- the row was written, for reading the table by hand.
CREATE TABLE acceptance_state (
  issue_id     VARCHAR(64)  NOT NULL,
  item_key     CHAR(64)     NOT NULL,
  n            INT          NOT NULL,
  state        VARCHAR(16)  NOT NULL,
  reason       VARCHAR(500) NOT NULL,
  by_principal VARCHAR(255) NOT NULL,
  at           TIMESTAMP(6) NOT NULL,
  PRIMARY KEY (issue_id, item_key)
);

-- Token usage (design §12.1): what harnesses report, one row per harness
-- request, keyed by the reporting principal and session and the harness's
-- own request id, so a batch sent again adds nothing. ("usage" is a
-- reserved word in SQL, hence token_usage.) Sessions are chosen by clients
-- and the CLI's is shared ("cli"), so the principal is part of the key.
--
-- at is the request's time from the source. A turn or session record
-- covers span_start to at; a request record has no span. A count is NULL
-- when the harness did not report it, which is not 0; cache_write_1h is
-- the part of cache_write written with a one-hour lifetime. added_at is
-- the server's time, which the daily cap (limits: usage_per_day) counts.
-- Which issue a row belongs to is computed when read, from the claim
-- events, and never stored. Attribution reads a session's rows by time
-- (token_usage_session), and its turn and session spans that end past a
-- time (token_usage_span); a span is at most 7 days, which bounds that.
CREATE TABLE token_usage (
  principal      VARCHAR(255) NOT NULL,
  session        VARCHAR(255) NOT NULL,
  request_id     VARCHAR(255) NOT NULL,
  machine        VARCHAR(255) NOT NULL,
  harness        VARCHAR(32)  NOT NULL,
  model          VARCHAR(128) NOT NULL,
  at             TIMESTAMP(6) NOT NULL,
  granularity    VARCHAR(16)  NOT NULL,
  span_start     TIMESTAMP(6) NULL,
  input          BIGINT       NULL,
  output         BIGINT       NULL,
  cache_write    BIGINT       NULL,
  cache_write_1h BIGINT       NULL,
  cache_read     BIGINT       NULL,
  added_at       TIMESTAMP(6) NOT NULL,
  PRIMARY KEY (principal, session, request_id)
);
CREATE INDEX token_usage_session ON token_usage (principal, session, at);
CREATE INDEX token_usage_span ON token_usage (principal, session, granularity, at);
CREATE INDEX token_usage_at ON token_usage (at);
CREATE INDEX token_usage_added ON token_usage (principal, added_at);

-- Files to issues (design §12 item 3): the paths an issue's work touched,
-- as clients found them in git (source 'commit'), and those a person or
-- agent declared (source 'declared'; a trailing '/' makes a directory
-- prefix). source is part of the key, so a path can be both, and a later
-- kind (such as predictions from similar issues) is a new source value,
-- not a new table. principal and session are who last wrote the row;
-- first_at and last_at the server's times. show orders commit paths by
-- last_at (issue_paths_recent); ready finds an issue's paths by the key's
-- issue_id prefix. Paths compare byte for byte, as git's do.
CREATE TABLE issue_paths (
  issue_id  VARCHAR(64)   NOT NULL,
  path      VARCHAR(1024) COLLATE "C" NOT NULL,
  source    VARCHAR(16)   NOT NULL,
  principal VARCHAR(255)  NOT NULL,
  session   VARCHAR(255)  NOT NULL,
  first_at  TIMESTAMP(6)  NOT NULL,
  last_at   TIMESTAMP(6)  NOT NULL,
  rev       BIGINT        NOT NULL,
  PRIMARY KEY (issue_id, path, source)
);
CREATE INDEX issue_paths_recent ON issue_paths (issue_id, source, last_at);

-- Memory (design §6): scoped records that agents and people keep across
-- sessions. A key names one record per scope: scope is team, project or
-- user, and owner is the principal for user scope and '' otherwise, so
-- the same key in two scopes, or two users' user scopes, is two records.
-- A user-scope record is its owner's alone; the server, not the key,
-- enforces that. author is who first remembered it, and the per-scope
-- limit (limits: memories_per_scope) counts by author (memories_author);
-- updated_by is the last writer. rev makes a replace a compare-and-swap,
-- as for issues. Forgetting a memory keeps its row as a tombstone
-- (deleted, its body, issue and tags cleared) so its rev keeps rising
-- when the key is remembered again, and a rev read before the forget
-- never matches. Keys and tags compare byte for byte. Recall and prime
-- read the newest first (memories_recent) and the memories linked to an
-- issue (memories_issue).
CREATE TABLE memories (
  id         VARCHAR(32)  NOT NULL,
  scope      VARCHAR(16)  NOT NULL,
  owner      VARCHAR(255) NOT NULL,
  mem_key    VARCHAR(255) COLLATE "C" NOT NULL,
  body       TEXT         NOT NULL,
  issue_id   VARCHAR(64)  NULL,
  pinned     BOOLEAN      NOT NULL DEFAULT FALSE,
  deleted    BOOLEAN      NOT NULL DEFAULT FALSE,
  author     VARCHAR(255) NOT NULL,
  updated_by VARCHAR(255) NOT NULL,
  created_at TIMESTAMP(6) NOT NULL,
  updated_at TIMESTAMP(6) NOT NULL,
  rev        BIGINT       NOT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX memories_key ON memories (scope, owner, mem_key);
CREATE INDEX memories_recent ON memories (updated_at);
CREATE INDEX memories_issue ON memories (issue_id);
CREATE INDEX memories_author ON memories (author, scope);

-- A memory's tags, one row each, like an issue's labels; prime matches
-- them against the labels of the caller's issues.
CREATE TABLE memory_tags (
  memory_id VARCHAR(32)  NOT NULL,
  tag       VARCHAR(255) COLLATE "C" NOT NULL,
  PRIMARY KEY (memory_id, tag)
);
CREATE INDEX memory_tags_tag ON memory_tags (tag);

-- Prices (design §12.1): a model's list rates from an effective time on,
-- one row per (model, effective_at). Rates are integer micro-dollars
-- (10^-6 USD) per million tokens, so 3 USD per million is 3000000 and a
-- cost is an exact integer sum, never a float. cache_write is the rate of
-- a five-minute cache write and cache_write_1h of a one-hour one. Costs
-- are computed when read, never stored, so a new price never rewrites
-- history: a usage record is priced by the newest row for its model whose
-- effective_at is no later than the record's time. Models compare byte
-- for byte, as harnesses report them. set_by and set_at say who last set
-- the row.
CREATE TABLE prices (
  model          VARCHAR(128) COLLATE "C" NOT NULL,
  effective_at   TIMESTAMP(6) NOT NULL,
  input          BIGINT       NOT NULL,
  output         BIGINT       NOT NULL,
  cache_write    BIGINT       NOT NULL,
  cache_write_1h BIGINT       NOT NULL,
  cache_read     BIGINT       NOT NULL,
  set_by         VARCHAR(255) NOT NULL,
  set_at         TIMESTAMP(6) NOT NULL,
  PRIMARY KEY (model, effective_at)
);

-- Human hours (design §12.1, aries log): a person's time on an issue, one
-- row per entry. principal is who logged it, for themselves; on_date is
-- the day worked (UTC) and seconds how long, at most a day. The account
-- comes from the issue when read, as for tokens, so moving an issue to
-- another account moves its hours too. Hours carry no money. Entries are
-- only inserted and deleted, never updated; each is an event on its
-- issue (hours.log, hours.delete), which keeps a deleted entry's history.
-- Reads go by issue (hours_issue), by person (hours_principal) and by day
-- (hours_on).
CREATE TABLE hours (
  id        VARCHAR(32)  NOT NULL,
  issue_id  VARCHAR(64)  NOT NULL,
  principal VARCHAR(255) NOT NULL,
  on_date   DATE         NOT NULL,
  seconds   INT          NOT NULL,
  note      TEXT         NOT NULL,
  logged_at TIMESTAMP(6) NOT NULL,
  PRIMARY KEY (id)
);
CREATE INDEX hours_issue ON hours (issue_id, on_date);
CREATE INDEX hours_principal ON hours (principal, on_date);
CREATE INDEX hours_on ON hours (on_date);

-- Subscription plans (design §12.1): a flat-rate plan's fee per seat per
-- month, its seats, and the principals whose usage it pays for, from a
-- month on until a later row of the same name supersedes it; a row with
-- no fee or no seats ends the plan. fee is integer micro-dollars (10^-6
-- USD), as prices' rates are, so the amortized cost a report splits from
-- fee x seats is exact. Costs are computed when read, never stored.
-- set_by and set_at say who last set the row. A row's principals are in
-- plan_principals, replaced with it.
CREATE TABLE plans (
  name       VARCHAR(64)  COLLATE "C" NOT NULL,
  from_month DATE         NOT NULL,
  fee        BIGINT       NOT NULL,
  seats      INT          NOT NULL,
  set_by     VARCHAR(255) NOT NULL,
  set_at     TIMESTAMP(6) NOT NULL,
  PRIMARY KEY (name, from_month)
);

CREATE TABLE plan_principals (
  name       VARCHAR(64)  COLLATE "C" NOT NULL,
  from_month DATE         NOT NULL,
  principal  VARCHAR(255) NOT NULL,
  PRIMARY KEY (name, from_month, principal)
);

-- Conflicts (design §18.7): a field of a replayed update that someone
-- else changed since the rev it was based on, parked rather than
-- applied. base, mine and theirs are the field's value at base_rev, in
-- the update and on the server when it was parked, as text; author and
-- session made the update, idem_key is its idempotency key, and
-- theirs_by and theirs_at made the server's value. A proposal (an
-- agent's or a person's suggested resolution: mine, theirs or text,
-- with why) is kept on the row, a later one replacing it. resolution is
-- mine, theirs, text or superseded once resolved, with who and when;
-- resolved_at is NULL while it is open. id rises with each conflict.
CREATE TABLE conflicts (
  id            BIGINT        NOT NULL,
  issue_id      VARCHAR(64)   NOT NULL,
  field         VARCHAR(32)   NOT NULL,
  base_rev      BIGINT        NOT NULL,
  base          TEXT          NOT NULL,
  mine          TEXT          NOT NULL,
  theirs        TEXT          NOT NULL,
  author        VARCHAR(255)  NOT NULL,
  session       VARCHAR(255)  NOT NULL,
  idem_key      VARCHAR(128)  NULL,
  theirs_by     VARCHAR(255)  NOT NULL,
  theirs_at     TIMESTAMP(6)  NOT NULL,
  parked_at     TIMESTAMP(6)  NOT NULL,
  proposal      VARCHAR(16)   NULL,
  proposal_text TEXT          NULL,
  proposal_why  VARCHAR(2000) NULL,
  proposed_by   VARCHAR(255)  NULL,
  proposed_at   TIMESTAMP(6)  NULL,
  resolution    VARCHAR(16)   NULL,
  resolved_by   VARCHAR(255)  NULL,
  resolved_at   TIMESTAMP(6)  NULL,
  PRIMARY KEY (id)
);
CREATE INDEX conflicts_issue ON conflicts (issue_id, resolved_at);
CREATE INDEX conflicts_author ON conflicts (author, resolved_at);
CREATE INDEX conflicts_open ON conflicts (resolved_at, parked_at);

-- Gates (design §20.3, decision 66): a gate is an issue of type 'gate',
-- and this table holds what is not an issue's, one row per gate. kind is
-- 'human' or 'ci'. approvers is a JSON array of the principals who may
-- pass it (or, for a ci gate, report its check), empty for the project's
-- admins. outcome is NULL while the gate is open and 'passed' or
-- 'waived' once it is closed, with decided_by, decided_at and reason; a
-- rearm sets it back to NULL. The gate's issue row says open or closed
-- too, and every change to either is made by the gate ops alone, in one
-- write, so the two never disagree. What a gate holds back depends on it
-- with an ordinary blocks edge.
--
-- A ci gate (§20.4; stage 6, slice G1) names a check (check_name, such as
-- 'build' or 'landed') and the issue whose branch the check runs on
-- (subject). A project has at most one ci gate for a subject and a
-- check, so a CI job that knows only its branch and its check finds one
-- gate (gates_subject). report is the last report a CI principal made,
-- passed or failed, with its evidence (a commit, a URL to the run, a
-- short reason), NULL until the first; a failure leaves the gate open,
-- and a pass closes it. Human gates keep check_name empty and subject
-- and report NULL.
CREATE TABLE gates (
  issue_id   VARCHAR(64)   NOT NULL,
  kind       VARCHAR(16)   NOT NULL,
  approvers  JSON          NOT NULL,
  outcome    VARCHAR(16)   NULL,
  decided_by VARCHAR(255)  NULL,
  decided_at TIMESTAMP(6)  NULL,
  reason     VARCHAR(2000) NOT NULL,
  rev        BIGINT        NOT NULL,
  check_name VARCHAR(64)   NOT NULL DEFAULT '',
  subject    VARCHAR(64)   NULL,
  report     JSON          NULL,
  PRIMARY KEY (issue_id)
);
CREATE INDEX gates_open ON gates (outcome);
CREATE INDEX gates_subject ON gates (subject, check_name);

-- Locks (design §20.5; stage 6, slice G2, decision 70): leased, fenced,
-- first-in-first-out mutexes and semaphores, named in the project and
-- tied to no issue.
--
-- locks has a row for each lock someone holds or waits for, with the
-- capacity its first acquire gave it. The row goes once nobody holds or
-- waits for the lock, so the names in use are what the locks limit
-- bounds, and the next acquire of the name makes it again.
--
-- lock_epochs has one row, id 1: the project's last epoch. Each grant of
-- any lock takes the next, so a lock's epochs only rise, across the
-- removal of its row too, and an epoch names one grant in the project.
--
-- lock_holders has a row for each live or lapsed hold, by the lock and
-- the hold's epoch; lease_us is the lease its acquire asked for, which a
-- renewal extends by. lock_waiters has a row for each place in a queue,
-- by the lock and seq, which orders the queue; a holder (principal,
-- session, machine) has one place in a lock's queue at most, and
-- lease_us is the hold's lease once granted. A row whose
-- expires_at has passed holds or waits for nothing: the next write to
-- the lock, or the reaper, ends it.
CREATE TABLE locks (
  name       VARCHAR(64)  NOT NULL,
  capacity   INT          NOT NULL,
  created_at TIMESTAMP(6) NOT NULL,
  created_by VARCHAR(255) NOT NULL,
  rev        BIGINT       NOT NULL,
  PRIMARY KEY (name)
);

CREATE TABLE lock_epochs (
  id    INT    NOT NULL,
  epoch BIGINT NOT NULL,
  PRIMARY KEY (id)
);

CREATE TABLE lock_holders (
  name       VARCHAR(64)  NOT NULL,
  epoch      BIGINT       NOT NULL,
  principal  VARCHAR(255) NOT NULL,
  session    VARCHAR(255) NOT NULL,
  machine    VARCHAR(255) NOT NULL,
  granted_at TIMESTAMP(6) NOT NULL,
  expires_at TIMESTAMP(6) NOT NULL,
  lease_us   BIGINT       NOT NULL,
  PRIMARY KEY (name, epoch)
);
CREATE INDEX lock_holders_expires ON lock_holders (expires_at);
CREATE INDEX lock_holders_principal ON lock_holders (principal);

CREATE TABLE lock_waiters (
  name       VARCHAR(64)  NOT NULL,
  principal  VARCHAR(255) NOT NULL,
  session    VARCHAR(255) NOT NULL,
  machine    VARCHAR(255) NOT NULL,
  seq        BIGINT       NOT NULL,
  joined_at  TIMESTAMP(6) NOT NULL,
  expires_at TIMESTAMP(6) NOT NULL,
  lease_us   BIGINT       NOT NULL,
  PRIMARY KEY (name, seq)
);
CREATE INDEX lock_waiters_expires ON lock_waiters (expires_at);
CREATE INDEX lock_waiters_principal ON lock_waiters (principal);
