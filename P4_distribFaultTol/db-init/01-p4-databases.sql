-- Runs once, on the primary, the first time the database cluster is created.
-- The replica copies everything from the primary, so it gets these too.
--
-- Every node gets its own database and its own user, and nobody else may
-- connect to it: the nodes share the replicated servers but not their data.
-- The password must match db_password in config.json.

CREATE ROLE auth     LOGIN PASSWORD 'p4-db-password';
CREATE ROLE course   LOGIN PASSWORD 'p4-db-password';
CREATE ROLE grade    LOGIN PASSWORD 'p4-db-password';
CREATE ROLE announce LOGIN PASSWORD 'p4-db-password';

CREATE DATABASE auth     OWNER auth;
CREATE DATABASE course   OWNER course;
CREATE DATABASE grade    OWNER grade;
CREATE DATABASE announce OWNER announce;

REVOKE ALL ON DATABASE auth     FROM PUBLIC;
REVOKE ALL ON DATABASE course   FROM PUBLIC;
REVOKE ALL ON DATABASE grade    FROM PUBLIC;
REVOKE ALL ON DATABASE announce FROM PUBLIC;
