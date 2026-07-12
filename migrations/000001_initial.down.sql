-- Reverse of 000001_initial.up.sql — clean teardown of the v1.0 schema.
-- Drop order is the exact reverse of creation order, respecting FKs:
-- dead_letter_queue references runs + steps; attempts references steps;
-- steps references runs; runs references workflows.

DROP TABLE IF EXISTS dead_letter_queue;
DROP TABLE IF EXISTS attempts;
DROP TABLE IF EXISTS steps;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS workflows;
