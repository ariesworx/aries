-- Conflicts (design §18.7) compare a replayed update, field by field,
-- against the state at the rev it was based on, which the event log
-- gives. rev is the issue's rev after the event, on the one event of
-- each write that changed the issue's row; it is NULL on every other
-- event, and on the events written before this migration, so a base
-- rev older than the issue's first stamped event cannot be compared.

ALTER TABLE events ADD COLUMN rev BIGINT NULL;
