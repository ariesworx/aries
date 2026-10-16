-- Trust rules (docs/design/identity.md §3.4 and §7, transport.md §7,
-- slice N5): which workload tokens sign in as which service principal.
--
-- A rule names a service principal, a workload provider of the server's
-- settings (workload: providers), and claims, an object of claim name to
-- the exact string the platform's token must carry: 1 to 16 of them,
-- with the platform's immutable ids among them, which the daemon checks
-- by the provider's kind. A token signs in by the one principal whose
-- active rules it satisfies. token_lifetime_seconds is how long what a
-- match gets lasts, at most the server's workload: max_token_lifetime.
-- A disabled rule matches nothing.
CREATE TABLE trust_rules (
  name                   VARCHAR(64)  COLLATE "C" NOT NULL,
  principal              VARCHAR(64)  NOT NULL REFERENCES principals (name),
  provider               VARCHAR(32)  NOT NULL,
  claims                 JSONB        NOT NULL,
  token_lifetime_seconds INTEGER      NOT NULL,
  state                  VARCHAR(16)  NOT NULL DEFAULT 'active',
  created_by             VARCHAR(255) NOT NULL,
  created_at             TIMESTAMP(6) NOT NULL,
  updated_at             TIMESTAMP(6) NOT NULL,
  rev                    BIGINT       NOT NULL,
  PRIMARY KEY (name),
  CONSTRAINT trust_rules_state CHECK (state IN ('active', 'disabled')),
  CONSTRAINT trust_rules_claims CHECK (jsonb_typeof(claims) = 'object'),
  CONSTRAINT trust_rules_lifetime CHECK (token_lifetime_seconds > 0)
);
CREATE INDEX trust_rules_principal ON trust_rules (principal);
CREATE INDEX trust_rules_provider ON trust_rules (provider) WHERE state = 'active';

-- A sign-in by a workload token names the rule it matched. Removing the
-- rule ends its sign-ins first, and clears the name; the events keep it.
ALTER TABLE grants ADD COLUMN trust_rule VARCHAR(64) COLLATE "C" NULL REFERENCES trust_rules (name);
ALTER TABLE grants ADD CONSTRAINT grants_trust_rule CHECK (trust_rule IS NULL OR flow = 'workload');
CREATE INDEX grants_trust_rule ON grants (trust_rule) WHERE trust_rule IS NOT NULL;
