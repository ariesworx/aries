-- A price may name a prompt length past which other rates apply (design
-- §12.1): a request whose prompt (input, cache writes and cache reads)
-- is over over_prompt tokens is priced at the tier_ rates. 0 means no
-- tier, and a price that already exists has none.
ALTER TABLE prices
  ADD COLUMN over_prompt        BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN tier_input         BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN tier_output        BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN tier_cache_write   BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN tier_cache_write_1h BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN tier_cache_read    BIGINT NOT NULL DEFAULT 0;
