#!/bin/sh
# Runs once, on first initialisation of the data directory, as the
# bootstrap superuser. Creates the two roles from ARCHITECTURE.md:
#   wallet_owner - owns the database and schema; used only by migrations
#   wallet_app   - used by the application; never owns objects
# Table-level grants for wallet_app (INSERT/UPDATE, never DELETE/TRUNCATE)
# are added by the schema migrations. The only default granted here is
# SELECT, which the app needs to verify the schema version at startup.
set -eu

: "${WALLET_DB:=wallet}"
: "${WALLET_OWNER_PASSWORD:?WALLET_OWNER_PASSWORD is required}"
: "${WALLET_APP_PASSWORD:?WALLET_APP_PASSWORD is required}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "${POSTGRES_DB:-postgres}" \
  -v db="$WALLET_DB" -v owner_pw="$WALLET_OWNER_PASSWORD" -v app_pw="$WALLET_APP_PASSWORD" <<'SQL'
CREATE ROLE wallet_owner LOGIN PASSWORD :'owner_pw';
CREATE ROLE wallet_app   LOGIN PASSWORD :'app_pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE :"db" OWNER wallet_owner;
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db" TO wallet_app;
SQL

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$WALLET_DB" <<'SQL'
ALTER SCHEMA public OWNER TO wallet_owner;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO wallet_app;
ALTER DEFAULT PRIVILEGES FOR ROLE wallet_owner IN SCHEMA public GRANT SELECT ON TABLES TO wallet_app;
SQL
