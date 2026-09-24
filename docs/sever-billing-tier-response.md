# sever billing-tier response contract

Responses JSON (including compact) adds `codex2api_billing`; Responses SSE
adds the same object under `response.codex2api_billing` when a response
envelope is present. Model output, standard `service_tier`, token usage and
the gateway's local pricing policy remain unchanged.

```json
{
  "version": 1,
  "service_tier": "priority",
  "source": "upstream_response",
  "requested_service_tier": "",
  "actual_service_tier": "priority",
  "local_billing_service_tier": "default"
}
```

`service_tier` reports the normalized execution tier. Prefer a known tier
observed in the upstream response (`source=upstream_response`); otherwise
report the effective request tier (`source=effective_request`). If neither
was observed, report an empty tier with `source=unobserved`. This does not
claim an unobserved upstream tier was actually delivered. Known `fast` is
normalized to `priority` for interoperability. Local billing follows the
existing policy and can differ from the reported execution tier.

NewAPI consumes successful terminal responses from its bound, signed
codex2api destination. Original user `service_tier: priority` OR reported
execution tier `priority` matches the existing Priority expression, once.
Chat Completions conversion retains its standard terminal `service_tier`,
which NewAPI accepts as a legacy report from the same trusted destination.

The codex2api admin request diagnostic adds `client_service_tier` before
normalization and rewriting. It distinguishes missing, null, string and
other JSON types. Known tier strings retain case and whitespace; arbitrary
strings are redacted. NewAPI separately preserves its own inbound value,
effective settlement tier and match source, so a relay's changes can be
compared without inventing original values for older logs.

This response contract covers HTTP Responses and upstream WebSocket events
converted to HTTP SSE. It does not add billing observation to NewAPI's
direct Realtime/WebSocket relay.
