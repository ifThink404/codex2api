# BPS attachment reuse and HTTP latency

On `main` and `sever`, requests handled by the gateway can share uploaded attachment
handles through the existing Redis runtime cache. The key includes the local
account ID, upstream account identity and content digest. No image bytes,
original filenames, authorization headers or proxy credentials are stored.
Memory-cache deployments keep the existing process-local behavior.

The bounded local cache remains the first layer. On a local miss, Redis is
checked before uploading. A per-attachment lease deduplicates uploads across
instances; it is not an account or user concurrency limit. The existing 100
image workers per request are unchanged. A waiting request respects cancellation.
Redis operations have a 250 ms deadline. A cache error disables further shared
lookups for that request and falls back to the existing local cache/upload path.
Leases expire after 65 seconds, slightly beyond the 60-second upload deadline.

A shared hit retains the original expiration time. Restarting a process or
reading from another instance does not reset the handle's age. Explicit
`file_not_found`/`file_expired` responses invalidate matching local and shared
entries, then use the existing single attachment retry. If the error identifies
a particular handle, other valid images are retained. Conditional shared deletion
prevents a late failure from deleting a newly uploaded replacement. Other
instances may retain a local copy until expiry; if rejected upstream, that copy
follows the same bounded invalidation/reupload path.

The default TTL stays at 30 minutes because a longer upstream attachment lifetime
has not been verified. Operators who have validated their upstream can set
`CODEX_BPS_ATTACHMENT_CACHE_TTL_MINUTES` (1–1440; invalid values use 30).
All instances should use the same setting. Increasing it does not guarantee that
the upstream will retain files for that long. No automatic cross-account reuse
or periodic keep-alive requests are introduced.

## New diagnostics

Under `upstream.bps_compat.timing`:

- `shared_cache_hits`: ready handles obtained from the shared backend. These
  also count as cache hits, rather than uploads/local cache misses.
- `shared_cache_waits` / `shared_cache_wait_ms`: requests joining another
  instance's upload and cumulative waiting duration. These can overlap local
  in-flight cache waits; do not sum them as sequential phases.
- `shared_cache_errors`: shared-backend errors, without raw error text.
- `last_inference_http`: phase snapshot for the last inference HTTP attempt,
  including failed attempts. Internal attachment retries replace this snapshot.
- `slowest_upload_http` and `upload_http_observations`: the longest attachment
  HTTP roundtrip's phase snapshot and the number of observed upload roundtrips.
- `first_token_mode_at_start`: gateway strict/loose setting at BPS execution
  entry; existing first-token and first-content timings keep their semantics.

HTTP phase snapshots contain only numeric timing, reuse and failure flags:

| Field | Meaning |
|---|---|
| `roundtrip_ms` | HTTP call start through response headers or transport error |
| `connection_acquire_ms` | HTTP call start through obtaining a connection; includes setup and pool waits |
| `connection_reused` | Whether an existing connection was obtained |
| `dns_ms` | DNS duration when standard HTTP trace callbacks are emitted |
| `tcp_ms` | Sum of observed standard TCP connection attempts; concurrent attempts can overlap |
| `dial_ms` | uTLS transport dial duration, including proxy negotiation when used |
| `tls_ms` | Observed TLS handshake duration |
| `request_write_ms` | Connection acquired through successful request-write callback, including HTTP/2 stream waits |
| `wrote_request_ms` | Request-write completion offset from HTTP call start |
| `first_response_byte_ms` | First response byte offset from HTTP call start |
| `response_wait_ms` | Successful request write through first response byte; includes network and server waiting |
| `source` | `httptrace` or the custom `utls` transport |

Missing callbacks remain absent; they are not recorded as zero. uTLS DNS and
proxy negotiation cannot be separated by these hooks and are included in
`dial_ms`. The custom pool records connection acquisition/reuse explicitly;
HTTP/2 callbacks supply request-write and first-response-byte timings.
Durations overlap: connection acquisition includes DNS/dial/TLS, and byte/write
offsets share a starting point. Do not add all fields together. No transport
retry, connection fingerprint or request-body behavior is changed by tracing.

The 2026-09-25 exported logs predate these measurements. The user reported
changing the proxy themselves; no same-account live before/after speedup has
been established from those logs.
