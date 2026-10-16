-- Expiring keys (docs/design/identity.md §3.3 and §4, transport.md
-- §11.3; slice S1): a key's end, who put the key there, and for a key a
-- sign-in made, the machine the client named.
--
-- A key with no end is a key from before, or one an admin added without
-- --expires: it lasts until removed. A key past its end authenticates
-- no one, over SSH or HTTPS; the daemon prunes it a while later, quietly.
-- Every key from before was put there by an admin or by `ariesd control
-- init`; which is not recorded, and the default says admin.
ALTER TABLE principal_keys ADD COLUMN expires_at TIMESTAMP(6) NULL;
ALTER TABLE principal_keys ADD COLUMN origin VARCHAR(16) NOT NULL DEFAULT 'admin';
ALTER TABLE principal_keys ADD COLUMN machine VARCHAR(64) NULL;
ALTER TABLE principal_keys ADD CONSTRAINT principal_keys_origin CHECK (origin IN ('admin', 'import', 'login', 'workload'));
CREATE INDEX principal_keys_expires_at ON principal_keys (expires_at) WHERE expires_at IS NOT NULL;
