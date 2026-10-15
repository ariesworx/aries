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
  theirs_at     TIMESTAMP(6)   NOT NULL,
  parked_at     TIMESTAMP(6)   NOT NULL,
  proposal      VARCHAR(16)   NULL,
  proposal_text TEXT          NULL,
  proposal_why  VARCHAR(2000) NULL,
  proposed_by   VARCHAR(255)  NULL,
  proposed_at   TIMESTAMP(6)   NULL,
  resolution    VARCHAR(16)   NULL,
  resolved_by   VARCHAR(255)  NULL,
  resolved_at   TIMESTAMP(6)   NULL,
  write_id      BIGINT        NOT NULL,
  PRIMARY KEY (id)
);
CREATE INDEX conflicts_issue ON conflicts (issue_id, resolved_at);
CREATE INDEX conflicts_author ON conflicts (author, resolved_at);
CREATE INDEX conflicts_open ON conflicts (resolved_at, parked_at);
