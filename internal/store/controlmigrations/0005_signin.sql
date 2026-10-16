-- Sign-ins (docs/design/transport.md §6.9, slice T2): what a sign-in at
-- an identity provider gives, through the server's own authorization
-- server.
--
-- A grant is one sign-in of a principal, on one machine and for one
-- client, until ends_at unless it is ended first: revoked_at and
-- end_reason are set together then. Its scopes are a space-separated
-- set of cli, mcp and ci. jkt is the thumbprint of the DPoP key its
-- tokens are bound to, NULL for a bearer grant (a remote connector's,
-- slice T3). identity is the identity it was made by, cleared when that
-- identity is removed, which ends the grant first. machine is what the
-- client said it is, trusted for nothing.
--
-- A token is one of a grant's: opaque, 256 random bits, of which only
-- the SHA-256 is kept, in hex. An access token lasts as its grant does;
-- a refresh token (T3) is spent once, at used_at. Deleting a grant
-- deletes its tokens.
CREATE TABLE grants (
  id         VARCHAR(32)  COLLATE "C" NOT NULL,
  principal  VARCHAR(64)  NOT NULL REFERENCES principals (name),
  kind       VARCHAR(16)  NOT NULL,
  client_id  VARCHAR(64)  NOT NULL,
  scopes     VARCHAR(64)  NOT NULL,
  jkt        VARCHAR(64)  COLLATE "C" NULL,
  resource   TEXT         NOT NULL,
  flow       VARCHAR(16)  NOT NULL,
  provider   VARCHAR(64)  NULL,
  identity   BIGINT       NULL REFERENCES identities (id),
  machine    VARCHAR(255) NOT NULL,
  created_at TIMESTAMP(6) NOT NULL,
  ends_at    TIMESTAMP(6) NOT NULL,
  revoked_at TIMESTAMP(6) NULL,
  end_reason VARCHAR(32)  NULL,
  PRIMARY KEY (id),
  CONSTRAINT grants_kind CHECK (kind IN ('person', 'service')),
  CONSTRAINT grants_flow CHECK (flow IN ('loopback', 'url', 'device', 'workload', 'card')),
  CONSTRAINT grants_ended CHECK ((revoked_at IS NULL) = (end_reason IS NULL)),
  CONSTRAINT grants_end CHECK (ends_at > created_at)
);
CREATE INDEX grants_principal ON grants (principal, ends_at);
CREATE INDEX grants_identity ON grants (identity) WHERE identity IS NOT NULL;

CREATE TABLE tokens (
  hash       CHAR(64)     COLLATE "C" NOT NULL,
  grant_id   VARCHAR(32)  COLLATE "C" NOT NULL REFERENCES grants (id) ON DELETE CASCADE,
  kind       VARCHAR(16)  NOT NULL DEFAULT 'access',
  expires_at TIMESTAMP(6) NOT NULL,
  used_at    TIMESTAMP(6) NULL,
  PRIMARY KEY (hash),
  CONSTRAINT tokens_kind CHECK (kind IN ('access', 'refresh')),
  CONSTRAINT tokens_hash CHECK (hash ~ '^[0-9a-f]{64}$')
);
CREATE INDEX tokens_grant ON tokens (grant_id);
