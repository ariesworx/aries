-- Identity (docs/design/identity.md §3, slice N1): what a principal is,
-- and the external identities a person signs in with.
--
-- A principal is a person or a service. Every principal from before is a
-- person, as the default makes it. A service answers to an owner, a
-- person; a person has none. The display name is what people see beside
-- the name; no event records its value.
ALTER TABLE principals ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'person';
ALTER TABLE principals ADD COLUMN display_name VARCHAR(128) NULL;
ALTER TABLE principals ADD COLUMN owner VARCHAR(64) NULL;
ALTER TABLE principals ADD CONSTRAINT principals_kind CHECK (kind IN ('person', 'service'));
ALTER TABLE principals ADD CONSTRAINT principals_service_owner CHECK ((kind = 'service') = (owner IS NOT NULL));
ALTER TABLE principals ADD CONSTRAINT principals_owner FOREIGN KEY (owner) REFERENCES principals (name);

-- An external identity of a person (decision 60): the address an admin
-- invited at a provider, and, once a sign-in binds it, the issuer and
-- subject the provider's token named. Until then it is an invitation,
-- which lapses at invite_until (decision 57: nobody is made a principal
-- at a first sign-in). Binding sets issuer, subject and bound_at, and
-- clears invite_until, in one write. last_sign_in is bookkeeping; the
-- sign-in itself is an event.
--
-- One person per external identity: a bound (issuer, subject) is unique.
-- One open invitation per address and provider: an unbound
-- (provider, address) is unique, the address compared without case.
-- The subject and the address compare byte for byte (COLLATE "C"), and
-- lower() folds ASCII letters only.
CREATE TABLE identities (
  id           BIGINT       GENERATED ALWAYS AS IDENTITY,
  principal    VARCHAR(64)  NOT NULL REFERENCES principals (name),
  provider     VARCHAR(64)  NOT NULL,
  issuer       TEXT         COLLATE "C" NULL,
  subject      VARCHAR(255) COLLATE "C" NULL,
  invited_as   VARCHAR(254) COLLATE "C" NOT NULL,
  invited_by   VARCHAR(255) NOT NULL,
  invited_at   TIMESTAMP(6) NOT NULL,
  invite_until TIMESTAMP(6) NULL,
  bound_at     TIMESTAMP(6) NULL,
  last_sign_in TIMESTAMP(6) NULL,
  rev          BIGINT       NOT NULL,
  PRIMARY KEY (id),
  CONSTRAINT identities_bound CHECK ((issuer IS NULL) = (subject IS NULL) AND (subject IS NULL) = (bound_at IS NULL)),
  CONSTRAINT identities_invite CHECK ((subject IS NULL) = (invite_until IS NOT NULL))
);
CREATE UNIQUE INDEX identities_subject ON identities (issuer, subject) WHERE subject IS NOT NULL;
CREATE UNIQUE INDEX identities_invited ON identities (provider, lower(invited_as)) WHERE subject IS NULL;
CREATE INDEX identities_principal ON identities (principal, id);
