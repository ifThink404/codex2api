# NewAPI upstream first-response timing

The setting **向 NewAPI 上报宽松首响应（不再提前提交 200）/ Report loose first
response to NewAPI (no early HTTP 200)** replaces early metadata passthrough. Its
stored/API key remains `codex_preflight_sse_passthrough_enabled` for
compatibility. It is off by default; an existing true value now enables reports,
never early writes.

Pre-content lifecycle and metadata SSE events are now always buffered until the
first content event, whatever this switch says: `continuousRetryPreflightPassthrough`
returns false unconditionally. Committing HTTP 200 early meant a `response.failed`
inside that window could no longer be returned under its real error code, nor take
silent account rotation or the over-window compaction retry. Those recover here.

Only codex2api's side of this contract has shipped. Enabling the setting on its
own is safe: it adds three response headers and changes nothing else, and a
gateway that does not implement the contract ignores them. The separate local
`first_token_mode` still controls codex2api usage-log timing. Reports always use
the loose event classifier (`isLooseFirstTokenResult`), excluding lifecycle,
error, terminal and heartbeat events. WS handshake/connection acquisition is not
a response event.

For the native Codex `/v1/responses` path (HTTP upstream or WS-to-HTTP bridge), the
winning attempt keeps its measurement private until the normal response commit, then
publishes it as:

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

A report is always this gateway's own measurement, held in the attempt's timing record
and never read back out of an upstream response header. A relay account's upstream is an
arbitrary base URL, possibly another codex2api with this switch on, so the relay branch
carries no timing record and also strips these three names from the upstream response as
soon as it arrives. That also makes the report survive the continue-thinking fold, which
replaces the upstream response before the buffered commit.

Commit boundaries that publish the headers, all of them existing write sites:

- streaming passthrough: immediately before the first non-deferred SSE frame
  reaches the downstream writer;
- streaming with continuous retry buffering: `commitResponsesStreamAttempt`,
  which also drops the staged values when the local replay commit fails;
- non-streaming: immediately before the aggregated JSON body is written.

The NewAPI-side parsing and display work is a separate follow-up and is not part
of this change. The contract it is expected to implement: accept reports only
from an authenticated request to an enabled matching policy target/key, for the
same request, user and channel; validate version, unique integer headers, bounds
and ordering, then strip these private headers. `other.frt` remains the actual
NewAPI first-frame latency. A new `other.upstream_first_response` object carries
`source`, `mode`, `ms` and `attempt_ms`; the list displays it as **Upstream first
response**, with observed first-frame timing in its title and in request details.
TPS, billing and existing aggregate metrics keep their original timing semantics.
Old, missing or invalid reports use the original display, and historical logs are
not rewritten. Until that lands, the headers are simply unread.
