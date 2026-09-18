# NewAPI upstream first-response timing

The setting **向 NewAPI 上报宽松首响应（不再提前提交 200）/ Report loose first
response to NewAPI (no early HTTP 200)** replaces early metadata passthrough. Its
stored/API key remains `codex_preflight_sse_passthrough_enabled` for
compatibility. It is off by default; an existing true value now enables reports,
never early writes.

Deploy both codex2api and the compatible NewAPI update, configure NewAPI's trusted
codex2api policy destination, then enable this setting. The separate local
`first_token_mode` still controls codex2api usage-log timing. Reports always use
the loose event classifier (`isLooseFirstTokenResult`), excluding lifecycle,
error, terminal and heartbeat events. WS handshake/connection acquisition is not
a response event.

For the native Codex `/v1/responses` path (HTTP upstream or WS-to-HTTP bridge), the
winning attempt stages these headers until the normal response commit:

| Header | Meaning |
| --- | --- |
| `X-Codex2API-Response-Timing` | `v1-loose` |
| `X-Codex2API-First-Response-Ms` | Milliseconds from codex2api handler entry to the winning attempt's first qualifying event, including admission and earlier retries |
| `X-Codex2API-Attempt-First-Response-Ms` | Milliseconds from the winning attempt's start to that event |

No extra request, heartbeat, flush or early HTTP 200 is introduced. Timing does
not change strict content detection, retry eligibility, turn-state masking or
response status. Values are monotonic elapsed durations, not cross-host timestamps.
Each attempt has independent staging; failed buffered attempts never publish
their timings. If an existing heartbeat already committed headers, reporting is
omitted and NewAPI falls back to its observed first-frame metric. Non-Codex/API
relay paths and native WS clients do not receive this HTTP timing contract.

Commit boundaries that publish the headers, all of them existing write sites:

- streaming passthrough: immediately before the first non-deferred SSE frame
  reaches the downstream writer;
- streaming with continuous retry buffering: `commitResponsesStreamAttempt`,
  which also drops the staged values when the local replay commit fails;
- non-streaming: immediately before the aggregated JSON body is written.

NewAPI accepts reports only from an authenticated request to an enabled matching
policy target/key, for the same request, user and channel. It validates version,
unique integer headers, bounds and ordering, then strips these private headers.
`other.frt` remains the actual NewAPI first-frame latency. The new
`other.upstream_first_response` object contains `source`, `mode`, `ms` and
`attempt_ms`; the list displays it as **Upstream first response**, with observed
first-frame timing in its title and in request details. TPS, billing and existing
aggregate metrics keep their original timing semantics. Old/missing/invalid
reports use the original display. Historical logs are not rewritten. The
NewAPI-side parsing and display work is tracked separately; unknown headers are
harmless to gateways that do not implement it.
