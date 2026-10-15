-- The control database (design §15): what belongs to the server rather
-- than to one project. Projects, principals, their public SSH keys,
-- memberships with roles, and an event log of every change to them.
-- Versions are recorded in control_migrations, not schema_migrations, so
-- a project's database and this one cannot be mistaken for each other.
--
-- Every name here is lowercase by validation except a key's fingerprint
-- and text, which are base64 and so compare byte for byte.

-- A project and the Dolt database that holds it. db_name is unique: no
-- two projects share a database. A project is written in state creating,
-- before its database exists, and marked active once it is migrated.
CREATE TABLE projects (
  id         CHAR(36)     NOT NULL,
  name       VARCHAR(64)  NOT NULL,
  prefix     VARCHAR(32)  NOT NULL,
  account    VARCHAR(64)  NOT NULL,
  db_name    VARCHAR(64)  NOT NULL,
  state      VARCHAR(16)  NOT NULL,
  created_by VARCHAR(255) NOT NULL,
  created_at DATETIME(6)  NOT NULL,
  updated_at DATETIME(6)  NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY projects_name (name),
  UNIQUE KEY projects_db_name (db_name)
);

-- A principal: one namespace for the whole server. A disabled principal
-- keeps its keys and memberships, and none of them work.
CREATE TABLE principals (
  name       VARCHAR(64)  NOT NULL,
  state      VARCHAR(16)  NOT NULL,
  created_by VARCHAR(255) NOT NULL,
  created_at DATETIME(6)  NOT NULL,
  updated_at DATETIME(6)  NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (name)
);

-- A principal's public keys. The fingerprint is the SHA256: form, so a
-- key belongs to one principal. `key` is the key type and its base64,
-- as the parser gave them back; the comment is for display only. A key
-- is added or removed, never changed, so it has no rev or write_id.
CREATE TABLE principal_keys (
  fingerprint VARCHAR(64)  COLLATE utf8mb4_0900_bin NOT NULL,
  principal   VARCHAR(64)  NOT NULL,
  `key`       TEXT         COLLATE utf8mb4_0900_bin NOT NULL,
  comment     VARCHAR(255) NOT NULL,
  added_by    VARCHAR(255) NOT NULL,
  added_at    DATETIME(6)  NOT NULL,
  PRIMARY KEY (fingerprint),
  KEY principal_keys_principal (principal, added_at)
);

-- Which principal may reach which project, and as what.
CREATE TABLE members (
  principal  VARCHAR(64)  NOT NULL,
  project    CHAR(36)     NOT NULL,
  role       VARCHAR(16)  NOT NULL,
  added_by   VARCHAR(255) NOT NULL,
  added_at   DATETIME(6)  NOT NULL,
  updated_at DATETIME(6)  NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (principal, project),
  KEY members_project (project, principal)
);

-- The event log, with the columns a project's has after its migrations
-- (the store writes both the same way), so every grant, revocation and
-- key change is history. A target is a project id, a principal name, a
-- key fingerprint, or PROJECT/PRINCIPAL for a membership.
CREATE TABLE events (
  seq          BIGINT       NOT NULL,
  at           DATETIME(6)  NOT NULL,
  principal    VARCHAR(255) NOT NULL,
  session      VARCHAR(255) NOT NULL,
  machine      VARCHAR(255) NOT NULL,
  op           VARCHAR(64)  NOT NULL,
  target       VARCHAR(128) NOT NULL,
  before_state JSON         NULL,
  after_state  JSON         NULL,
  idem_key     VARCHAR(128) NULL,
  idem_args    CHAR(64)     NULL,
  idem_result  JSON         NULL,
  PRIMARY KEY (seq),
  UNIQUE KEY events_idem (principal, idem_key),
  KEY events_target (target, seq),
  KEY events_at (at)
);
