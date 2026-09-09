#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "rollback error: $*" >&2
  exit 1
}

app_dir=${CODEX2API_APP_DIR:-/opt/ai-stack/apps/codex2api}
router_dir=${CODEX2API_ROUTER_DIR:-$app_dir/runtime-router/router}
router_container=${CODEX2API_ROUTER_CONTAINER:-codex2api-router}
admin_forward_container=${CODEX2API_ADMIN_FORWARD_CONTAINER:-codex2api-admin-forward}
admin_forward_url=${CODEX2API_ADMIN_FORWARD_URL:-http://127.0.0.1:18095}
verify_count=${CODEX2API_VERIFY_COUNT:-6}
state_file=$app_dir/release-state.json
[[ -f "$state_file" ]] || die "missing $state_file"

current_release=$(jq -er '.current.release' "$state_file")
current_container=$(jq -er '.current.container' "$state_file")
current_config=$(jq -er '.current.config' "$state_file")
current_version=$(jq -er '.current.version' "$state_file")
current_image_id=$(jq -er '.current.image_id' "$state_file")
rollback_release=$(jq -er '.rollback.release' "$state_file")
rollback_container=$(jq -er '.rollback.container' "$state_file")
rollback_config=$(jq -er '.rollback.config' "$state_file")
rollback_version=$(jq -er '.rollback.version' "$state_file")
rollback_image_id=$(jq -er '.rollback.image_id' "$state_file")
expected_version=${rollback_version#v}

[[ -d "$current_release" && -d "$rollback_release" ]] || die "release directory missing"
[[ -f "$router_dir/$current_config" && -f "$router_dir/$rollback_config" ]] || die "router config missing"
docker inspect "$current_container" "$rollback_container" >/dev/null
bash "$(dirname "${BASH_SOURCE[0]}")/verify-postgres.sh" "$current_container" "$rollback_container"
admin_secret=$(docker inspect "$rollback_container" --format '{{json .Config.Env}}' |
  jq -er 'map(select(startswith("ADMIN_SECRET=")))[0] | split("=")[1:] | join("=")')

restore_current() {
  echo "rollback verification failed; restoring $current_config" >&2
  docker unpause "$current_container" >/dev/null 2>&1 || true
  docker start "$current_container" >/dev/null 2>&1 || true
  ln -sfn "$current_config" "$router_dir/nginx.conf"
  docker exec "$router_container" nginx -t >/dev/null 2>&1 || true
  docker exec "$router_container" nginx -s reload >/dev/null 2>&1 || true
  docker exec "$admin_forward_container" nginx -t >/dev/null 2>&1 || true
  docker exec "$admin_forward_container" nginx -s reload >/dev/null 2>&1 || true
}
trap restore_current ERR

docker unpause "$rollback_container" >/dev/null 2>&1 || true
docker start "$rollback_container" >/dev/null 2>&1 || true
ln -sfn "$rollback_config" "$router_dir/nginx.conf"
docker exec "$router_container" nginx -t >/dev/null
docker exec "$admin_forward_container" nginx -t >/dev/null
docker exec "$router_container" nginx -s reload >/dev/null
docker exec "$admin_forward_container" nginx -s reload >/dev/null

consecutive=0
attempts=0
while (( consecutive < verify_count && attempts < verify_count * 8 )); do
  attempts=$((attempts + 1))
  version=$(curl --max-time 15 -fsS -H "X-Admin-Key: $admin_secret" \
    "$admin_forward_url/api/admin/system/update" | jq -r '.current_version' || true)
  if [[ "$version" == "$expected_version" ]]; then consecutive=$((consecutive + 1)); else consecutive=0; fi
  sleep 0.25
done
(( consecutive >= verify_count )) || die "production route did not stabilize on $expected_version"

docker stop -t 30 "$current_container" >/dev/null
for ((i=0; i<verify_count; i++)); do
  version=$(curl --max-time 15 -fsS -H "X-Admin-Key: $admin_secret" \
    "$admin_forward_url/api/admin/system/update" | jq -er '.current_version')
  [[ "$version" == "$expected_version" ]] || die "route regressed after pausing former current container"
done

state_tmp=$(mktemp "$app_dir/.release-state.XXXXXX")
jq -n \
  --arg current_release "$rollback_release" --arg current_container "$rollback_container" --arg current_config "$rollback_config" \
  --arg current_version "$rollback_version" --arg current_image_id "$rollback_image_id" \
  --arg rollback_release "$current_release" --arg rollback_container "$current_container" --arg rollback_config "$current_config" \
  --arg rollback_version "$current_version" --arg rollback_image_id "$current_image_id" \
  '{current:{release:$current_release,container:$current_container,config:$current_config,version:$current_version,image_id:$current_image_id},
    rollback:{release:$rollback_release,container:$rollback_container,config:$rollback_config,version:$rollback_version,image_id:$rollback_image_id}}' \
  >"$state_tmp"
chmod 600 "$state_tmp"
mv -f "$state_tmp" "$state_file"
ln -sfn "$rollback_release" "$app_dir/current-release"

trap - ERR
echo "rollback_ok version=$expected_version current=$rollback_container rollback=$current_container"
