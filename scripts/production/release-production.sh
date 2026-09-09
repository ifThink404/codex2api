#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: release-production.sh --release-dir DIR --router-config FILE

This is the single production release entrypoint. It serializes releases,
performs the verified cutover, prints the retention plan, and then enforces the
one-current plus one-rollback retention policy.
EOF
}

die() {
  echo "release error: $*" >&2
  exit 1
}

app_dir=${CODEX2API_APP_DIR:-/opt/ai-stack/apps/codex2api}
bin_dir=${CODEX2API_BIN_DIR:-$app_dir/bin}
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
for script in cutover-release.sh prune-releases.sh; do
  [[ -x "$bin_dir/$script" ]] || die "missing executable $bin_dir/$script"
done
command -v flock >/dev/null || die "missing command: flock"

exec 9>"$app_dir/.production-release.lock"
flock -n 9 || die "another production release or cleanup is already running"

echo "release_step=verified_cutover"
"$bin_dir/cutover-release.sh" \
  --release-dir "$release_dir" \
  --router-config "$router_config"

echo "release_step=retention_preview"
"$bin_dir/prune-releases.sh"

echo "release_step=retention_apply"
"$bin_dir/prune-releases.sh" --apply

echo "release_ok=true"
