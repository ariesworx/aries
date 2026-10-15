-- Agents: the live registry behind "who". One row per (principal,
-- session), touched when a session connects and while it renews; who
-- lists the rows seen recently. harness is empty when the client did not
-- say which agent it runs under.

CREATE TABLE agents (
  principal VARCHAR(255) NOT NULL,
  session   VARCHAR(255) NOT NULL,
  machine   VARCHAR(255) NOT NULL,
  harness   VARCHAR(32)  NOT NULL,
  started   TIMESTAMP(6)  NOT NULL,
  last_seen TIMESTAMP(6)  NOT NULL,
  rev       BIGINT       NOT NULL,
  write_id  BIGINT       NOT NULL,
  PRIMARY KEY (principal, session)
);
CREATE INDEX agents_last_seen ON agents (last_seen);
