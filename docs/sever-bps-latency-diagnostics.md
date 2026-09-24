# BPS latency diagnostics on sever

`upstream.bps_compat.timing` separates attachment preparation from the
inference HTTP request. Image preparation now uploads at most ten images
concurrently per request, with no account-wide or global upload semaphore.
Two requests on the same account may each have ten uploads in flight. File
attachments retain their existing preparation path. All image uploads finish
before inference begins; results are written back in original input order.
Failure cancels and joins all image workers before returning. Cache policy,
image detail, tool call association and `first_token_ms` semantics are unchanged.

| Field | Meaning |
| --- | --- |
| `pre_inference_ms` | From entry into the BPS executor to the first inference HTTP roundtrip, including metadata/body preparation and attachments. Absent if inference never starts. |
| `image_prepare_ms`, `file_prepare_ms`, `tool_bridge_ms` | Cumulative wall time in each preparation pass, including internal retries. Image/file preparation includes decoding, cache resolution, upload and reference rewriting. |
| `upload_requests`, `upload_failures`, `upload_bytes` | Upload attempts, failed attempts, and raw attachment bytes submitted to the upload helper; includes failed attempts and reuploads, excludes multipart overhead. |
| `upload_ms`, `upload_max_ms` | Sum and maximum of complete upload-helper durations, including multipart construction, HTTP response and result validation. |
| `upload_last_http_status` | Last upload response status; absent when no response was received for the last attempt. |
| `cache_hits` | Resolved entries that were already ready when looked up. |
| `cache_misses` | Calls that elected to perform an upload, including capacity-overflow uncached uploads. |
| `cache_waits`, `cache_wait_ms` | Number and cumulative duration of lookups joining an in-flight upload, including canceled/failed waits. These are separate from ready hits. |
| `attachment_retries` | Internal retries after an upstream missing/expired attachment response. |
| `inference_attempts` | Number of inference HTTP roundtrips started by this BPS execution. |
| `inference_headers_total_ms` | Sum of inference HTTP response-header delays for attempts that returned a response. Does not include attachments or response-body consumption. |
| `last_inference_headers_ms` | Last inference HTTP roundtrip start to response headers; includes network, connection acquisition, request transmission and upstream waiting. |
| `last_inference_first_event_ms` | Last inference HTTP roundtrip start to the first observed named/typed SSE event. |
| `last_inference_first_content_ms` | Last inference HTTP roundtrip start to a content event under the existing strict first-token predicate. Includes reasoning/tool content, not just visible answer text. |

Milliseconds are truncated at each observation. Phase durations overlap:
`upload_ms` and `cache_wait_ms` sum work performed during image/file preparation.
With concurrent image uploads, both sums can exceed preparation wall time;
do not subtract them from first-token latency or add all fields together.
The first preparation pass is included in `pre_inference_ms`.
Header/event/content durations share the inference start
reference and are not consecutive durations either.

All counters except `last_*` accumulate across the one permitted internal
attachment retry. Existing `images.uploaded` / `images.upload_reused` continue
to describe the final preparation pass. Last-inference markers reset at each
retry. Missing markers mean unobserved, not zero latency. JSON responses such
as compact have a headers measurement but do not manufacture SSE markers.

Preparation failures now publish the BPS diagnostic before returning, with
`bps_image_preparation`, `bps_file_preparation`, or `bps_tool_attachment_bridge`
as the error stage and `send_phase=before_payload` for the inference request.
Attachment uploads may already have occurred even when inference never starts.

Only timing, sizes, counters and status codes are collected. No filenames,
image data, cache keys, file handles, prompts or credentials are added. The
request-local collector serializes under its own mutex. It does not alter
the transport's replay decisions or first-token timeout.

Interpretation examples:

- Large `upload_ms`: investigate attachment network/processing latency and
  number/size of new images on the selected proxy.
- Large `cache_wait_ms`: this request waited for other requests uploading the
  same account-scoped attachment. `images.upload_reused` alone previously hid
  that wait.
- Small attachment times with large inference first-content time: inspect the
  inference transport/upstream wait; do not blame uploads from image count alone.
- Large preparation time with small upload/cache time: local body processing
  and reference rewriting need examination.

These fields cannot reconstruct phase durations for logs recorded before
deployment. The local 2026-09-24 export analysis is under
`F:\codex\reports\latency-audit-20260924\report.md`.
