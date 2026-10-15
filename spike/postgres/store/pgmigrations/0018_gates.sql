-- Gates (design §20.3, decision 66): a gate is an issue of type 'gate',
-- and this table holds what is not an issue's, one row per gate. kind is
-- 'human' (the only kind of stage 6's first slice). approvers is a JSON
-- array of the principals who may pass it, empty for the project's
-- admins. outcome is NULL while the gate is open and 'passed' or
-- 'waived' once it is closed, with decided_by, decided_at and reason; a
-- rearm sets it back to NULL. The gate's issue row says open or closed
-- too, and every change to either is made by the gate ops alone, in one
-- write, so the two never disagree. What a gate holds back depends on it
-- with an ordinary blocks edge.

CREATE TABLE gates (
  issue_id   VARCHAR(64)   NOT NULL,
  kind       VARCHAR(16)   NOT NULL,
  approvers  JSON          NOT NULL,
  outcome    VARCHAR(16)   NULL,
  decided_by VARCHAR(255)  NULL,
  decided_at TIMESTAMP(6)   NULL,
  reason     VARCHAR(2000) NOT NULL,
  rev        BIGINT        NOT NULL,
  write_id   BIGINT        NOT NULL,
  PRIMARY KEY (issue_id)
);
CREATE INDEX gates_open ON gates (outcome);
