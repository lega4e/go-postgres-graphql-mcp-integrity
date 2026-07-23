-- M0 fixture schema: the worked example from SPEC.md §5.2, hand-written.
--
-- Loaded by postgres.WithInitScripts at container init. The M0 milestone does
-- not yet generate this DDL (that is M1); the point of M0 is to prove the test
-- harness and confirm PostgreSQL 19 SQL/PGQ is available end to end.

CREATE TABLE persons (
    id    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name  text NOT NULL,
    email text
);

CREATE TABLE follows (
    source_id uuid NOT NULL REFERENCES persons (id),
    target_id uuid NOT NULL REFERENCES persons (id),
    PRIMARY KEY (source_id, target_id)
);
CREATE INDEX follows_target_idx ON follows (target_id);   -- reverse traversal

CREATE PROPERTY GRAPH app_graph
  VERTEX TABLES (
    persons LABEL person PROPERTIES (id, name, email)      -- key re-listed
  )
  EDGE TABLES (
    follows SOURCE KEY (source_id) REFERENCES persons (id)
            DESTINATION KEY (target_id) REFERENCES persons (id)
            LABEL follows PROPERTIES (source_id, target_id)
  );
