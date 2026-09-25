# BPS conversation hints without client session IDs

The account's `codex_fingerprint_mode=session` (Device + session) now opts ordinary BPS requests into a stable outbound session hint when the client has no explicit session identity. Native Codex, relay accounts, and the off/device/full modes retain their existing behavior. No default setting changes.

The hint is scoped to the authenticated caller, optional installation/device ID, inbound UA, leading instructions/system messages and first user message. Verified NewAPI user identity is included; direct callers are separated by their Codex2API key. A signed installation ID takes precedence over client hints. A generic relay UA does not count as a device ID. Device IDs may be absent: in that case the caller and opening still partition the hint, with the weaker source recorded in diagnostics.

BPS then applies its existing account/profile cache partitioning. Within that partition, appending turns keeps task_id stable. Word now sends durable UUIDv7 task/turn metadata without Session-Id or prompt_cache_key; missing turn IDs are inferred from the latest user boundary so tool continuations retain their turn. Other profiles retain their existing Session-Id, prompt_cache_key and generated-turn behavior. Changes to models or tool declarations alone do not change the opening hint. The existing account-identity epoch mapping still applies.

Explicit session/cache IDs and resolved roots take precedence. Conflicting identity, local-only affinity overrides, related/internal requests, compaction and previous_response_id continuations are not inferred. This hint is never inserted into inbound metadata, NewAPI signatures, root ownership, session-window accounting, or authorization. It does not change retry or policy-refusal handling.

Diagnostics appear under `upstream.bps_compat.inferred_session`: result, skip reason, source, device_source, ua_included and heuristic. They contain no raw prompt, device ID, UA, or user identity. The actual outbound identifiers remain in the existing outbound identity diagnostic.

This is a heuristic, not a recovered conversation ID. Two independent chats with the same user/device/UA and identical opening share a hint. Changing the UA or opening instructions, or omitting/truncating the opening history, can change or disable the hint. For guaranteed separation and continuity, callers must send their own stable session ID. Shared direct API keys cannot distinguish end users who provide identical hints.

Validation uses local mock transports only: cross-turn reuse, caller/device/opening/account/profile separation, explicit-ID precedence, exclusions, logging and unchanged disabled modes. It does not establish upstream cache hits or explain upstream 403 policy errors.
