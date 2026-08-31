#!/usr/bin/env bash
# Print the `go test` selector args for one test-race shard so heavy packages
# do not share a 2-core runner. Admin and database both have long tails under
# -race, so each is split in two by test-name initial. The complementary
# regexes cover every Test* function without silently skipping tests.
# The `rest` shard is everything except the dedicated shards.
set -euo pipefail

shard="${1:-}"
case "$shard" in
  admin-a-l)
    echo "-run ^Test[A-L] ./admin"
    ;;
  admin-m-z)
    echo "-run ^Test[^A-L] ./admin"
    ;;
  database-a-l)
    echo "-run ^Test[A-L] ./database"
    ;;
  database-m-z)
    echo "-run ^Test[^A-L] ./database"
    ;;
  proxy)
    echo ./proxy/...
    ;;
  promptfilter)
    echo ./security/promptfilter
    ;;
  rest)
    go list ./... | grep -Ev '/(admin|database)($|/)' | grep -Ev '/proxy($|/)' | grep -Ev '/security/promptfilter$'
    ;;
  *)
    echo "unknown test-race shard: ${shard}" >&2
    exit 1
    ;;
esac
