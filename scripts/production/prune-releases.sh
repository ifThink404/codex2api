#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: prune-releases.sh [--apply]

Without --apply, prints the exact deletion plan. The current release, the one
rollback release, router/admin-forward containers, their images, shared data,
and the stable router runtime are always protected.
EOF
}

die() {
  echo "prune error: $*" >&2
  exit 1
}

apply=false
case ${1:-} in
  '') ;;
  --apply) apply=true ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

app_dir=${CODEX2API_APP_DIR:-/opt/ai-stack/apps/codex2api}
state_file=$app_dir/release-state.json
releases_dir=$(realpath -e "$app_dir/releases")
router_dir=$(realpath -e "${CODEX2API_ROUTER_DIR:-$app_dir/runtime-router/router}")
router_runtime=$(realpath -e "$(dirname "$router_dir")")
[[ -f "$state_file" ]] || die "missing $state_file"

current_release=$(realpath -e "$(jq -er '.current.release' "$state_file")")
rollback_release=$(realpath -e "$(jq -er '.rollback.release' "$state_file")")
current_container=$(jq -er '.current.container' "$state_file")
rollback_container=$(jq -er '.rollback.container' "$state_file")
current_config=$(jq -er '.current.config' "$state_file")
rollback_config=$(jq -er '.rollback.config' "$state_file")
current_image_id=$(jq -er '.current.image_id' "$state_file")
rollback_image_id=$(jq -er '.rollback.image_id' "$state_file")
router_image_id=$(docker inspect codex2api-router --format '{{.Image}}')

for release in "$current_release" "$rollback_release"; do
  [[ "$release" == "$releases_dir/"* ]] || die "protected release is outside $releases_dir: $release"
done
[[ "$current_release" != "$rollback_release" ]] || die "current and rollback releases are identical"

declare -A protected_containers=(
  ["$current_container"]=1
  ["$rollback_container"]=1
  [codex2api-router]=1
  [codex2api-admin-forward]=1
  [codex2api-postgres]=1
  [codex2api-redis]=1
)
declare -A protected_images=(
  ["$current_image_id"]=1
  ["$rollback_image_id"]=1
  ["$router_image_id"]=1
)

containers_to_delete=()
while IFS= read -r name; do
  [[ -n "$name" ]] || continue
  [[ ${protected_containers[$name]+yes} ]] || containers_to_delete+=("$name")
done < <(docker ps -a --format '{{.Names}}' | grep -E '^codex2api-v[0-9]' | grep -v -- '-sqlite-rollback$' | sort)

releases_to_delete=()
while IFS= read -r path; do
  resolved=$(realpath -e "$path")
  [[ "$resolved" == "$current_release" || "$resolved" == "$rollback_release" ]] || releases_to_delete+=("$resolved")
done < <(find "$releases_dir" -maxdepth 1 -mindepth 1 -type d -print | sort)

configs_to_delete=()
while IFS= read -r path; do
  name=$(basename "$path")
  [[ "$name" == "$current_config" || "$name" == "$rollback_config" || "$name" == nginx.conf ]] ||
    configs_to_delete+=("$path")
done < <(find "$router_dir" -maxdepth 1 -type f -name 'nginx*.conf*' -print | sort)

backup_files=()
if [[ -d "$app_dir/data/config-backups" ]]; then
  while IFS= read -r path; do backup_files+=("$path"); done < <(find "$app_dir/data/config-backups" -mindepth 1 -print | sort -r)
fi

runtime_items_to_delete=()
while IFS= read -r path; do
  name=$(basename "$path")
  [[ "$name" == router || "$name" == admin-forward-logs ]] || runtime_items_to_delete+=("$path")
done < <(find "$router_runtime" -maxdepth 1 -mindepth 1 -print | sort)

images_to_delete=()
while IFS='|' read -r ref image_id; do
  [[ -n "$ref" && "$ref" != '<none>:<none>' ]] || continue
  [[ ${protected_images[$image_id]+yes} ]] || images_to_delete+=("$ref")
done < <(docker image ls --no-trunc --format '{{.Repository}}:{{.Tag}}|{{.ID}}' | grep -E '^fr-migration/codex2api')

echo "KEEP release $current_release"
echo "KEEP release $rollback_release"
echo "KEEP container $current_container"
echo "KEEP container $rollback_container"
echo "KEEP router runtime $(dirname "$router_dir")"
for item in "${containers_to_delete[@]}"; do echo "DELETE container $item"; done
for item in "${releases_to_delete[@]}"; do echo "DELETE release $item"; done
for item in "${configs_to_delete[@]}"; do echo "DELETE router-config $item"; done
for item in "${backup_files[@]}"; do echo "DELETE config-backup $item"; done
for item in "${runtime_items_to_delete[@]}"; do echo "DELETE router-runtime-artifact $item"; done
for item in "${images_to_delete[@]}"; do echo "DELETE image $item"; done

if ! $apply; then
  echo "dry_run=true"
  exit 0
fi

for container in "${containers_to_delete[@]}"; do
  docker unpause "$container" >/dev/null 2>&1 || true
  docker stop -t 30 "$container" >/dev/null 2>&1 || true
  docker rm "$container" >/dev/null
done

for release in "${releases_to_delete[@]}"; do
  [[ "$release" == "$releases_dir/"* ]] || die "refusing release deletion outside $releases_dir"
  [[ "$release" != "$current_release" && "$release" != "$rollback_release" ]] || die "refusing protected release deletion"
  find "$release" -depth -delete
done

for config in "${configs_to_delete[@]}"; do
  resolved_parent=$(realpath -e "$(dirname "$config")")
  [[ "$resolved_parent" == "$router_dir" ]] || die "refusing config deletion outside $router_dir"
  rm -f -- "$config"
done

for backup in "${backup_files[@]}"; do
  [[ "$backup" == "$app_dir/data/config-backups/"* ]] || die "refusing backup deletion outside config-backups"
  if [[ -d "$backup" ]]; then rmdir "$backup" 2>/dev/null || true; else rm -f -- "$backup"; fi
done

for item in "${runtime_items_to_delete[@]}"; do
  [[ "$item" == "$router_runtime/"* ]] || die "refusing runtime deletion outside $router_runtime"
  [[ $(basename "$item") != router && $(basename "$item") != admin-forward-logs ]] || die "refusing protected runtime deletion"
  if [[ -d "$item" ]]; then find "$item" -depth -delete; else rm -f -- "$item"; fi
done

for ref in "${images_to_delete[@]}"; do
  docker image rm "$ref" >/dev/null 2>&1 || true
done

echo "prune_ok=true"
