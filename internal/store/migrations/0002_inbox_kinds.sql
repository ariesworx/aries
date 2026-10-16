-- The four inbox kinds that had bare names take noun.verb ones, as the
-- rest have, before v1.0.0 fixes them (decision 88): an item written
-- before this migration reads as one written after it.
UPDATE inbox SET kind = 'issue.assigned' WHERE kind = 'assigned';
UPDATE inbox SET kind = 'issue.mentioned' WHERE kind = 'mention';
UPDATE inbox SET kind = 'issue.handoff' WHERE kind = 'handoff';
UPDATE inbox SET kind = 'conflict.parked' WHERE kind = 'conflict';
