-- The command surface the M14 generated client's mutations call, reduced to the
-- two this suite uses. It is a copy of the relevant half of test/m14's fixture
-- rather than a shared file on purpose: what is under test here is the handle,
-- and a suite that broke because M14 grew a fifth function would be reporting
-- the wrong thing.
CREATE SCHEMA app;

CREATE FUNCTION app.add_person(person_name text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO persons (name) VALUES (person_name);
    RETURN person_name;
END;
$$;

CREATE FUNCTION app.follow(from_name text, to_name text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO follows (source_id, target_id)
    SELECT s.id, t.id FROM persons s, persons t
    WHERE s.name = from_name AND t.name = to_name;
END;
$$;
