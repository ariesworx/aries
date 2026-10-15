-- CI gates (design §20.3 and §20.4; stage 6, slice G1). A gate of kind
-- 'ci' names a check (check_name, such as 'build' or 'landed') and the
-- issue whose branch the check runs on (subject); its approvers are the
-- principals that may report the check. A project has at most one ci
-- gate for a subject and a check, so a CI job that knows only its branch
-- and its check finds one gate (gates_subject). report is the last
-- report a CI principal made, passed or failed, with its evidence (a
-- commit, a URL to the run, a short reason), NULL until the first; a
-- failure leaves the gate open, and a pass closes it. Human gates keep
-- check_name empty and subject and report NULL.

ALTER TABLE gates ADD COLUMN check_name VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE gates ADD COLUMN subject VARCHAR(64) NULL;
ALTER TABLE gates ADD COLUMN report JSON NULL;
CREATE INDEX gates_subject ON gates (subject, check_name);
