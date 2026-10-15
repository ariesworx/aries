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
  created_at DATETIME(6)  NOT NULL,
  created_by VARCHAR(255) NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (name)
);

CREATE TABLE lock_epochs (
  id       INT    NOT NULL,
  epoch    BIGINT NOT NULL,
  write_id BIGINT NOT NULL,
  PRIMARY KEY (id)
);

CREATE TABLE lock_holders (
  name       VARCHAR(64)  NOT NULL,
  epoch      BIGINT       NOT NULL,
  principal  VARCHAR(255) NOT NULL,
  session    VARCHAR(255) NOT NULL,
  machine    VARCHAR(255) NOT NULL,
  granted_at DATETIME(6)  NOT NULL,
  expires_at DATETIME(6)  NOT NULL,
  lease_us   BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (name, epoch),
  KEY lock_holders_expires (expires_at),
  KEY lock_holders_principal (principal)
);

CREATE TABLE lock_waiters (
  name       VARCHAR(64)  NOT NULL,
  principal  VARCHAR(255) NOT NULL,
  session    VARCHAR(255) NOT NULL,
  machine    VARCHAR(255) NOT NULL,
  seq        BIGINT       NOT NULL,
  joined_at  DATETIME(6)  NOT NULL,
  expires_at DATETIME(6)  NOT NULL,
  lease_us   BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (name, seq),
  KEY lock_waiters_expires (expires_at),
  KEY lock_waiters_principal (principal)
);
