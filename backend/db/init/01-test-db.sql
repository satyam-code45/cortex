-- Runs once, on first container start, via /docker-entrypoint-initdb.d.
-- Creates the throwaway database the integration tests truncate, so that
-- `docker compose up -d && make migrate && make check` works on a fresh clone.
-- The schema itself is applied by `make migrate-test`.
CREATE DATABASE cortex_test OWNER cortex;
