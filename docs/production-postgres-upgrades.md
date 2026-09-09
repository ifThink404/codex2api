# fr-netcup-new PostgreSQL production upgrades

Production uses PostgreSQL (`codex2api-postgres`), not SQLite. Keep the current
memory cache configuration unless a separate cache migration is requested.

The production upgrade entrypoint is
`/opt/ai-stack/apps/codex2api/bin/release-production.sh`. Its maintained copies
are in `scripts/production/`; deploy all five scripts together. Do not use the
interactive root `deploy.sh` to replace this installation.

1. Merge the verified upstream release into `codex/production-main`, preserve
   local changes, run tests and commit the release source.
2. Build using `scripts/build-release.sh --version vX.Y.Z-fr-YYYYMMDD.N`.
3. Take a PostgreSQL custom-format `pg_dump` before starting the candidate.
   Validate its archive inventory and record its checksum. Keep a protected copy
   of release metadata, environment and upgrade scripts. Never log secrets.
4. Derive the candidate environment from the running production container,
   preserving all `DATABASE_*` credentials, database identity and cache settings.
   Do not use `.env.sqlite.example` or stale pre-migration release files.
5. Start a candidate on a separate loopback port and the existing Docker network,
   preserving image assets, log mounts and a trusted CA bundle. Verify PostgreSQL
   guards, direct health, version, account loading and API authentication.
6. Create candidate `release.json` and a router configuration targeting the
   candidate by name. Preview cleanup before invoking the production entrypoint:
   `release-production.sh --release-dir DIR --router-config FILE`.
7. Verify routed health/version, PostgreSQL sessions and new usage writes.

Both cutover and rollback reject SQLite, missing database fields, mismatched
database identities and unavailable PostgreSQL. Cleanup only selects versioned
application containers; PostgreSQL, Redis and the SQLite migration rollback
container are not cleanup candidates. Backups must never be placed under release
directories subject to retention cleanup.

Application rollback reuses the same live PostgreSQL database; it is not a
database restore. Review schema compatibility before deploying. Do not switch
back to the stale SQLite database or restore a pre-upgrade dump after accepting
new writes without a separate reconciliation plan.
