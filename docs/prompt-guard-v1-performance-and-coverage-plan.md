# Prompt Guard V1 architecture and coverage

This document describes the public design constraints for Prompt Guard on V1
proxy endpoints. It intentionally avoids deployment-specific branches,
credentials, account inventories, and production procedures.

## Goals

- Apply the same policy semantics across Responses, Chat Completions, Messages,
  Images, and supported HTTP, SSE, and WebSocket transports.
- Keep the disabled path lightweight: do not parse or copy request bodies when
  no enabled feature needs prompt text.
- Evaluate current user-controlled input separately from system, developer,
  history, tool, attachment, and session context.
- Keep audit persistence and optional enrichment outside the response-critical
  path wherever the selected enforcement policy permits it.
- Preserve unknown advanced-config fields so newer servers remain compatible
  with older administration clients.

## Processing model

```text
request or frame
  -> protocol-specific prompt extraction
  -> bounded normalization and decoding
  -> source-aware local rules
  -> optional external model review
  -> configured enforcement mode
  -> asynchronous audit and risk aggregation
  -> upstream forwarding or local rejection
```

The local rule decision and external review scope are independent settings.
External review can cover all extractable requests, only local warn/block
candidates, or only local block decisions. Existing configurations without a
scope retain the historical all-request behavior.

## Source handling

Current user input is eligible for enforcement. Other sources are disabled or
shadow-only by default and may contribute audit evidence without independently
blocking a request. Deployments that change these defaults should document the
trust boundary for every newly enforced source.

## Safety and privacy constraints

- Never persist authorization headers, cookies, API keys, access tokens, or
  unredacted request bodies.
- Bound decoded size, decode passes, attachment size, response buffering, and
  external-review input length.
- Treat external review as data egress. Operators must explicitly configure the
  provider, credentials, scope, timeout, and failure policy.
- External-review failures must follow the configured fail-open or fail-closed
  policy; they must not silently invent a successful decision.
- Risk aggregation and identity-aware enforcement are optional. A connector to
  another gateway must not be required for local filtering or model review.

## Protocol coverage

The regression matrix should include:

| Endpoint family | HTTP | SSE | WebSocket | Multipart |
|---|---:|---:|---:|---:|
| Responses | yes | yes | yes | n/a |
| Chat Completions | yes | yes | where supported | n/a |
| Messages | yes | yes | n/a | n/a |
| Images | yes | where supported | n/a | edits |
| Realtime | n/a | n/a | yes | n/a |

For retrying requests, tests should verify that request correlation is stable
while each upstream policy incident receives its own incident identifier.

## Required validation

- Safe input, audit-only matches, warn decisions, local blocks, external-review
  clears, external-review blocks, and review failures.
- Missing scores serialize as `null`; genuine zero scores serialize as `0`.
- Encoded input, long input limits, invalid UTF-8, malformed JSON, multipart
  extraction, fragmented SSE content, and WebSocket frame boundaries.
- SQLite and PostgreSQL migrations, paginated queries, cleanup, and transaction
  rollback for compound audit writes.
- Disabled-path benchmarks and representative enabled-path latency and memory
  measurements.
- Frontend tests for configuration round trips, scope selection, null/zero
  display, pagination, and exact incident lookup.

## Extension points

- OpenAI-compatible moderation or chat-completions review providers.
- Optional signed identity/enforcement adapters, including NewAPI deployments.
- Optional semantic sidecars, session correlation, attachment parsing, output
  scanning, and rule-intelligence jobs.

Extensions must remain disabled by default unless the project documents their
resource cost, privacy implications, failure behavior, and protocol coverage.
