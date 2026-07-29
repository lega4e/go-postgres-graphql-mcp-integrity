CREATE TABLE companies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL
);

CREATE TABLE persons (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    email_address text UNIQUE,
    age integer,
    score numeric(10,2),
    active boolean,
    "createdAt" timestamptz,
    meta jsonb,
    nicknames text[]
);
CREATE INDEX persons_age_idx ON persons (age);
CREATE INDEX persons_created_idx ON persons USING btree ("createdAt");

CREATE TABLE follows (
    source_id uuid NOT NULL REFERENCES persons (id),
    target_id uuid NOT NULL REFERENCES persons (id),
    PRIMARY KEY (source_id, target_id)
);
CREATE INDEX follows_target_idx ON follows (target_id);

CREATE TABLE works_at (
    source_id uuid NOT NULL REFERENCES persons (id),
    target_id uuid NOT NULL REFERENCES companies (id),
    PRIMARY KEY (source_id, target_id)
);
CREATE INDEX works_at_target_idx ON works_at (target_id);

CREATE PROPERTY GRAPH app_graph
  VERTEX TABLES (
    companies LABEL company PROPERTIES (id, name)
            LABEL actor PROPERTIES (id, name),
    persons LABEL person PROPERTIES (id, name, email_address, age, score, active, "createdAt", meta, nicknames)
            LABEL actor PROPERTIES (id, name)
  )
  EDGE TABLES (
    follows SOURCE KEY (source_id) REFERENCES persons (id)
            DESTINATION KEY (target_id) REFERENCES persons (id)
            LABEL follows PROPERTIES (source_id, target_id),
    works_at SOURCE KEY (source_id) REFERENCES persons (id)
            DESTINATION KEY (target_id) REFERENCES companies (id)
            LABEL works_at PROPERTIES (source_id, target_id)
  );
