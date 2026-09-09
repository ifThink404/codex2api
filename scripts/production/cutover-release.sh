#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: cutover-release.sh --release-dir DIR --router-config FILE

Required release.json fields:
  version, container, port, previous_release, previous_container

The script keeps the previous container serving while nginx reloads, verifies
the routed backend repeatedly, checks a trusted CA bundle in both containers,
and only then pauses the previous container and updates release-state.json.
EOF
}

die() {
  echo "cutover error: $*" >&2
  if [[ ${route_switched:-false} == true ]] && declare -F restore_previous_route >/dev/null; then
    restore_previous_route
  fi
  exit 1
}

app_dir=${CODEX2API_APP_DIR:-/opt/ai-stack/apps/codex2api}
router_dir=${CODEX2API_ROUTER_DIR:-$app_dir/runtime-router/router}
router_container=${CODEX2API_ROUTER_CONTAINER:-codex2api-router}
admin_forward_container=${CODEX2API_ADMIN_FORWARD_CONTAINER:-codex2api-admin-forward}
admin_forward_url=${CODEX2API_ADMIN_FORWARD_URL:-http://127.0.0.1:18095}
verify_count=${CODEX2API_VERIFY_COUNT:-6}
route_switched=false

release_dir=
router_config=
while [[ $# -gt 0 ]]; do
  case "$1" in
    --release-dir) release_dir=${2:-}; shift 2 ;;
    --router-config) router_config=${2:-}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$release_dir" && -n "$router_config" ]] || { usage >&2; exit 2; }
release_dir=$(realpath -e "$release_dir")
releases_dir=$(realpath -e "$app_dir/releases")
[[ "$release_dir" == "$releases_dir/"* ]] || die "release directory is outside $releases_dir"
metadata=$release_dir/release.json
[[ -f "$metadata" ]] || die "missing $metadata"

new_version=$(jq -er '.version' "$metadata")
expected_version=${new_version#v}
new_container=$(jq -er '.container' "$metadata")
new_port=$(jq -er '.port' "$metadata")
previous_release=$(realpath -e "$(jq -er '.previous_release' "$metadata")")
previous_container=$(jq -er '.previous_container' "$metadata")
[[ "$previous_release" == "$releases_dir/"* ]] || die "previous release is outside $releases_dir"
[[ "$new_container" != "$previous_container" ]] || die "new and previous containers are identical"

if [[ "$router_config" != /* ]]; then
  router_config=$router_dir/$router_config
fi
router_config=$(realpath -e "$router_config")
router_dir_real=$(realpath -e "$router_dir")
[[ "$router_config" == "$router_dir_real/"* ]] || die "router config is outside $router_dir_real"
new_config=$(basename "$router_config")
previous_config=$(readlink "$router_dir/nginx.conf")
[[ -n "$previous_config" && -f "$router_dir/$previous_config" ]] || die "current router config is invalid"
grep -Eq "^[[:space:]]*server[[:space:]]+${new_container}:8080([[:space:];]|$)" "$router_config" ||
  die "router config must target the candidate container directly: $new_container:8080"

for command in curl docker jq realpath; do
  command -v "$command" >/dev/null || die "missing command: $command"
done
docker inspect "$new_container" "$previous_container" "$router_container" "$admin_forward_container" >/dev/null
bash "$(dirname "${BASH_SOURCE[0]}")/verify-postgres.sh" "$previous_container" "$new_container"

admin_secret=$(docker inspect "$new_container" --format '{{json .Config.Env}}' |
  jq -er 'map(select(startswith("ADMIN_SECRET=")))[0] | split("=")[1:] | join("=")')

verify_ca_bundle() {
  local container=$1
  local label=$2
  local probe_dir
  local bundle
  local found=''

  probe_dir=$(mktemp -d "${TMPDIR:-/tmp}/codex2api-ca-probe.XXXXXX")
  for bundle in \
    /etc/ssl/certs/ca-certificates.crt \
    /etc/pki/tls/certs/ca-bundle.crt \
    /etc/ssl/ca-bundle.pem \
    /etc/ssl/cert.pem; do
    rm -f "$probe_dir/bundle"
    if docker cp "$container:$bundle" "$probe_dir/bundle" >/dev/null 2>&1 && [[ -s "$probe_dir/bundle" ]]; then
      found=$bundle
      break
    fi
  done
  rm -rf -- "$probe_dir"
  [[ -n "$found" ]] || die "$label container has no readable trusted CA bundle"
  echo "${label}_ca_bundle=$found"
}

restore_previous_route() {
  echo "cutover failed; restoring $previous_config" >&2
  docker unpause "$previous_container" >/dev/null 2>&1 || true
  docker start "$previous_container" >/dev/null 2>&1 || true
  ln -sfn "$previous_config" "$router_dir/nginx.conf"
  docker exec "$router_container" nginx -t >/dev/null 2>&1 || true
  docker exec "$router_container" nginx -s reload >/dev/null 2>&1 || true
  docker exec "$admin_forward_container" nginx -t >/dev/null 2>&1 || true
  docker exec "$admin_forward_container" nginx -s reload >/dev/null 2>&1 || true
  route_switched=false
}
trap restore_previous_route ERR

docker unpause "$previous_container" >/dev/null 2>&1 || true
docker unpause "$new_container" >/dev/null 2>&1 || true
docker start "$previous_container" >/dev/null 2>&1 || true
docker start "$new_container" >/dev/null 2>&1 || true
[[ $(docker inspect "$new_container" --format '{{.State.Status}}') == running ]] || die "$new_container is not running"
verify_ca_bundle "$new_container" candidate
verify_ca_bundle "$previous_container" rollback
curl --max-time 10 -fsS "http://127.0.0.1:$new_port/health" >/dev/null
direct_version=$(curl --max-time 15 -fsS -H "X-Admin-Key: $admin_secret" \
  "http://127.0.0.1:$new_port/api/admin/system/update" | jq -er '.current_version')
[[ "$direct_version" == "$expected_version" ]] || die "direct version $direct_version != $expected_version"

route_switched=true
ln -sfn "$new_config" "$router_dir/nginx.conf"
docker exec "$router_container" nginx -t >/dev/null
docker exec "$admin_forward_container" nginx -t >/dev/null
docker exec "$router_container" nginx -s reload >/dev/null
docker exec "$admin_forward_container" nginx -s reload >/dev/null

consecutive=0
attempts=0
max_attempts=$((verify_count * 8))
while (( consecutive < verify_count && attempts < max_attempts )); do
  attempts=$((attempts + 1))
  routed_version=$(curl --max-time 15 -fsS -H "X-Admin-Key: $admin_secret" \
    "$admin_forward_url/api/admin/system/update" | jq -r '.current_version' || true)
  if [[ "$routed_version" == "$expected_version" ]]; then
    consecutive=$((consecutive + 1))
  else
    consecutive=0
  fi
  sleep 0.25
done
(( consecutive >= verify_count )) || die "production route did not stabilize on $expected_version"

# The update endpoint reports the latest upstream version, not this container's
# release identity. Rollback verification must use the immutable release metadata.
previous_version=$(jq -er '.version' "$previous_release/release.json")

docker stop -t 30 "$previous_container" >/dev/null
for ((i=0; i<verify_count; i++)); do
  routed_version=$(curl --max-time 15 -fsS -H "X-Admin-Key: $admin_secret" \
    "$admin_forward_url/api/admin/system/update" | jq -er '.current_version')
  [[ "$routed_version" == "$expected_version" ]] || die "route regressed after pausing previous container"
done

new_image_id=$(docker inspect "$new_container" --format '{{.Image}}')
previous_image_id=$(docker inspect "$previous_container" --format '{{.Image}}')
state_tmp=$(mktemp "$app_dir/.release-state.XXXXXX")
jq -n \
  --arg current_release "$release_dir" --arg current_container "$new_container" --arg current_config "$new_config" \
  --arg current_version "$new_version" --arg current_image_id "$new_image_id" \
  --arg rollback_release "$previous_release" --arg rollback_container "$previous_container" --arg rollback_config "$previous_config" \
  --arg rollback_version "$previous_version" --arg rollback_image_id "$previous_image_id" \
  '{current:{release:$current_release,container:$current_container,config:$current_config,version:$current_version,image_id:$current_image_id},
    rollback:{release:$rollback_release,container:$rollback_container,config:$rollback_config,version:$rollback_version,image_id:$rollback_image_id}}' \
  >"$state_tmp"
chmod 600 "$state_tmp"
mv -f "$state_tmp" "$app_dir/release-state.json"
ln -sfn "$release_dir" "$app_dir/current-release"
ln -sfn "$app_dir/bin/rollback-release.sh" "$app_dir/rollback-current.sh"
ln -sfn "$app_dir/bin/rollback-release.sh" "$app_dir/rollback-codex2api.sh"

route_switched=false
trap - ERR
echo "cutover_ok version=$expected_version current=$new_container rollback=$previous_container"
