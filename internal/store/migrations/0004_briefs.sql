-- A brief, recorded by whoever starts a session for an issue (design
-- §22): a comment of kind 'brief' holds its record line and text, and
-- this row its fields, its hash and the claim it is joined to. state is
-- 'pending' until a start joins it to a claim ('bound'), or until a
-- later brief replaces it ('replaced'). A claim, one epoch of an
-- issue's, has at most one brief; NULL epochs never collide.
CREATE TABLE briefs (
  comment_id      VARCHAR(32)  NOT NULL,
  issue_id        VARCHAR(64)  NOT NULL,
  harness         VARCHAR(128) NOT NULL,
  model           VARCHAR(128) NOT NULL,
  effort          VARCHAR(16)  NOT NULL,
  session         VARCHAR(255) NOT NULL,
  template        VARCHAR(128) NOT NULL,
  sha256          CHAR(64)     NOT NULL,
  state           VARCHAR(16)  NOT NULL,
  claim_principal VARCHAR(255) NULL,
  claim_session   VARCHAR(255) NULL,
  claim_epoch     BIGINT       NULL,
  bound_at        TIMESTAMP(6) NULL,
  PRIMARY KEY (comment_id)
);
CREATE INDEX briefs_issue ON briefs (issue_id, state);
CREATE UNIQUE INDEX briefs_claim ON briefs (issue_id, claim_epoch);
