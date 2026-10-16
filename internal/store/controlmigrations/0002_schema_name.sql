-- A project's schema is named for what it is (decision 79, slice P2):
-- since the move to Postgres it is a schema in the server's one
-- database, which the daemon names from the project's name, not a
-- database of its own.
ALTER TABLE projects RENAME COLUMN db_name TO schema_name;
ALTER INDEX projects_db_name RENAME TO projects_schema_name;
