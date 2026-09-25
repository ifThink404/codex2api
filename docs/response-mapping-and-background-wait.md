# Response mapping and background wait recovery

## Local response mapping

`turn-state mapping unavailable` used to hide response ID, turn-state and
protocol metadata storage failures behind the same stream-read error. It does
not establish that the upstream disconnected. Existing historical logs without
an underlying cause cannot identify which mapping operation failed.

Response/turn-state writes now have a five-second budget (previously one second),
bounded by the request's own deadline. Protocol pair reads/writes also have this
bound. A successful persistent pair is cached within the request, with owner,
root, account, credential identity and failover generation included in its key.
Misses and errors are not cached. No unpersisted alias or raw upstream handle is
published as a recovery shortcut. Reprocessing an issued response ID is
idempotent within its binding.

Mapping failures are local terminal errors: they do not penalize an upstream
account or replay an inference through another account. HTTP/SSE/WebSocket use
the existing local-failure terminal handling. Usage and service-error diagnostics
include `response_mapping` (at most 16 entries): operation, reason, duration and
SQLSTATE when available. `upstream.error_source` is `gateway` and `error_stage`
is `response_mapping`. Raw SQL/driver text and mapped values are omitted.

## Background wait fallback

With relaxed mode enabled, a background request whose parent owner/window
disappears, changes, or times out while waiting may use the existing temporary
passive-request admission flow. This uses a separate scheduling key and outbound
epoch, normal model/account/group/capacity checks, and existing context cleanup.
It never changes the parent's durable binding. The temporary binding is released
at request completion. The original shared wait budget is not restarted.

Strict mode, request cancellation, explicit window tickets, identity conflicts,
failed ownership storage reads and invalid migrated context retain their errors.
Preserve-input mode still requires context that can be safely reused; ordinary
restart mode removes account-bound opaque continuation data. Chat and Messages
admission checks translate a copy before checking Responses-style input.

Diagnostics record `root_account_wait=relaxed_fallback`, the original wait
details and `relaxed_fallback.reason=passive_wait_*`. Responses, compact, Chat,
Messages and WebSocket call the same wait/fallback helper. HTTP handler tests
verify fresh selection and cleanup after a wait timeout; owner changes, strict
mode, explicit tickets and cancellation are tested separately.

These are shared fixes suitable for main. They do not import sever's separate
changes to ingress identity or session validation.
