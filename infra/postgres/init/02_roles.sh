#!/usr/bin/env bash
# =============================================================================
# Least-privilege roles. Runs once at first container start, after 01_schema.sql.
# A shell script (not .sql) because it needs passwords from environment variables
# instead of hard-coding them in the repo.
#   payflow_app       -> the simulator / services: read + write rows, no DDL
#   payflow_readonly  -> analysts, BI, ad-hoc queries
# =============================================================================
set -euo pipefail

: "${APP_DB_USER:?APP_DB_USER must be set}"
: "${APP_DB_PASSWORD:?APP_DB_PASSWORD must be set}"
: "${READONLY_DB_USER:?READONLY_DB_USER must be set}"
: "${READONLY_DB_PASSWORD:?READONLY_DB_PASSWORD must be set}"

psql -v ON_ERROR_STOP=1 \
     --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
     -v app_user="$APP_DB_USER" -v app_pw="$APP_DB_PASSWORD" \
     -v ro_user="$READONLY_DB_USER" -v ro_pw="$READONLY_DB_PASSWORD" <<-'EOSQL'
    CREATE ROLE :"app_user" LOGIN PASSWORD :'app_pw';
    CREATE ROLE :"ro_user"  LOGIN PASSWORD :'ro_pw';

    -- Nobody gets anything by default.
    REVOKE ALL ON SCHEMA public FROM PUBLIC;

    GRANT USAGE ON SCHEMA payments, simulation TO :"app_user", :"ro_user";

    -- App: DML only. No DELETE on payments data (we never delete money records).
    GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA payments   TO :"app_user";
    GRANT SELECT, INSERT         ON ALL TABLES IN SCHEMA simulation TO :"app_user";
    GRANT USAGE ON ALL SEQUENCES IN SCHEMA payments TO :"app_user";

    -- Read-only: SELECT only.
    GRANT SELECT ON ALL TABLES IN SCHEMA payments, simulation TO :"ro_user";

    -- Make the app's search_path point at our schema.
    ALTER ROLE :"app_user" SET search_path = payments, public;
    ALTER ROLE :"ro_user"  SET search_path = payments, public;
EOSQL

echo "roles created: ${APP_DB_USER}, ${READONLY_DB_USER}"
