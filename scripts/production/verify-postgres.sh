#!/usr/bin/env bash
set -euo pipefail

[[ $# -eq 2 ]] || { echo 'Usage: verify-postgres.sh CURRENT CANDIDATE' >&2; exit 2; }
pg_container=${CODEX2API_PG_CONTAINER:-codex2api-postgres}
current_config=
for container in "$@"; do
  config=$(docker inspect "$container" --format '{{json .Config.Env}}' | jq -ce '
    map(capture("^(?<key>[^=]+)=(?<value>.*)$")) | from_entries |
    {DATABASE_DRIVER, DATABASE_HOST, DATABASE_PORT, DATABASE_USER,
     DATABASE_PASSWORD, DATABASE_NAME, DATABASE_SSLMODE}')
  jq -e --arg host "$pg_container" '
    .DATABASE_DRIVER == "postgres" and .DATABASE_HOST == $host and
    ([.DATABASE_PORT, .DATABASE_USER, .DATABASE_PASSWORD, .DATABASE_NAME,
      .DATABASE_SSLMODE] | all(. != null and . != ""))' <<< "$config" >/dev/null || {
      echo "PG guard: invalid or incomplete PostgreSQL configuration: $container" >&2
      exit 1
    }
  if [[ -n "$current_config" && "$config" != "$current_config" ]]; then
    echo 'PG guard: candidate and current database configurations differ' >&2
    exit 1
  fi
  current_config=$config
done
[[ $(docker inspect "$pg_container" --format '{{.State.Status}}') == running ]] || {
  echo 'PG guard: database container is not running' >&2
  exit 1
}
database=$(jq -r '.DATABASE_NAME' <<< "$current_config")
username=$(jq -r '.DATABASE_USER' <<< "$current_config")
docker exec "$pg_container" pg_isready -U "$username" -d "$database" >/dev/null
docker exec "$pg_container" psql -X -v ON_ERROR_STOP=1 -U "$username" -d "$database" -Atqc 'SELECT 1' | grep -qx 1
echo "postgres_guard_ok=true database_container=$pg_container"
