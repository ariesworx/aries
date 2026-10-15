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
  at             TIMESTAMP(6)  NOT NULL,
  read_at        TIMESTAMP(6)  NULL,
  write_id       BIGINT       NOT NULL,
  PRIMARY KEY (id)
);
CREATE INDEX inbox_to ON inbox (to_principal, read_at);
