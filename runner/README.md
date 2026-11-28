# AIShie Loopback MCP Runner

TypeScript runner for Handout v1.2 §4.4. It runs one Vercel AI SDK 7 `streamText` tool loop per request and connects directly to AIShie Core over MCP Streamable HTTP. The HTTP server binds **127.0.0.1 only**.

## Run

```sh
npm ci
npm run typecheck
npm test
npm run build
node dist/index.js                 # RUNNER_ENTRY used by the Go manager
```

- `RUNNER_PORT`: loopback port, default `3001`.
- `EGRESS_PROXY`: optional HTTP(S) proxy for model-provider traffic (undici `ProxyAgent`).
- Bedrock with an empty `model.api_key` uses the AWS SigV4 default credential chain; a non-empty key uses Bedrock bearer authentication.
- Development server: `npm run dev` (the same executable through `tsx watch`).

Smoke check:

```sh
curl http://127.0.0.1:3001/healthz
# {"status":"ok"}
```

## Internal API

### `POST /run`

Accepts the Handout JSON DTO and returns `application/x-ndjson`: `step`, `tool_start`, `tool_end`, `text_delta`, then exactly one `done` or `error` event. Request JSON is limited to 40 MiB and unknown top-level/model/limit fields are rejected. `messages` supports text and image parts (`media_type`, `data_base64`); assistant image parts are rejected. Go remains responsible for attachment count, byte limits, and fetching attachment bytes.

`allowed_tools` is the effective allowlist computed by Go after its seat-permission policy. An empty list offers no tools. The Runner independently applies built-in denies, configured denies, `readOnlyHint`, binding, guards, budgets, and result sanitation. An MCP tool without `annotations.readOnlyHint === true` is conservatively treated as a write.

The response `usage` is the SDK's aggregate across all steps. With AI SDK 7, `isStepCount` is the successor of the Handout's `stepCountIs`; `usage` is the documented multi-step sum and `totalUsage` is its deprecated alias.

### `POST /probe`

Preferred body:

```json
{"model":{"adapter":"anthropic","provider":"anthropic","model_id":"claude-sonnet-4-5","api_key":"…","hosted":true},"timeout_ms":25000}
```

A bare model object is also accepted. The probe makes one non-streaming one-token call and returns only:

```json
{"result":"ok|key_refused|model_not_found|key_accepted|unreachable","http_status":null,"provider_code":null,"latency_ms":123}
```

No provider message/body, model header, API key, or routing object is returned or logged. `provider_code` is null unless it matches `^[A-Za-z0-9_.:-]{1,64}$`.

## Security behavior

- A fresh MCP client is created per run and closed in `finally`, along with both egress dispatchers. MCP initializes with protocol `2025-11-25`; envelopes are read from `structuredContent`, falling back to JSON in text content.
- Core loopback traffic bypasses `EGRESS_PROXY`. Model traffic uses the configured proxy. Redirects are never followed.
- `model.hosted: true` requires HTTPS and a public destination. Literal and DNS-resolved loopback, private, link-local, CGNAT, multicast, documentation, and reserved addresses are refused, including at the connection lookup (DNS rebinding defense). `hosted: false` is the operator-controlled development/self-hosted escape hatch.
- `course_id`, `idempotency_key`, and `revises` are removed from model schemas and model input. Writes are validated against Core's original schema **after** the final runtime binding. A write number is consumed only after schema, Decides, and SeatGuard checks pass.
- Write keys are `tool:{conversation}:{message}:{attempt}:{n}`, hashed as `tool:<sha256>` above 200 characters. Canonical `(tool, effective arguments)` repeats reuse the first key. Calls execute sequentially so key assignment follows model call order.
- `action_decide` and `action_review` require `confirm_required`. Member writes cannot target self, principal, opener, or their other delegates. Role-wide writes check protected people and the roster, failing closed. Guard reads use direct MCP calls and consume neither model-tool nor write budgets.
- Every `download_url`/`downloadUrl` is recursively removed. Results are UTF-8 byte-limited using actual JSON-encoded size, preserving `status` and `error` and placing overflow in `result_truncated`.
- Search excerpts set `sources_stated: false` until real document content is read. Ordered, deduplicated sources (maximum 20) include file/page/slide/part where known. Safe public material/search titles alone can become `tool_end.target`; draft, archived, rubric, submission, and unpublished-assignment titles are withheld.

## Local chat

```sh
npm run dev:chat -- \
  --core http://127.0.0.1:8080 \
  --token ais_... \
  --course 0192f3c1-... \
  --provider anthropic \
  --model claude-sonnet-4-5
```

Use `--tools name1,name2` to change the explicit allowlist and `--allow-writes` to permit listed write tools. Other options: `--base-url`, `--region`, `--timeout`, and `--api-key` (the provider's normal environment variable is preferable). `/clear` resets history and `/quit` exits. No Go control plane, PostgreSQL, or Vault is needed.

## Test coverage

Vitest covers real security behavior and NDJSON contracts, required bound-field validation, forged binding replacement, Decides/SeatGuard (including bulk), stable keys, budgets, URL stripping and byte truncation, source provenance, all five adapters, OpenRouter body injection, SSRF/redirects, a real local `EGRESS_PROXY`, safe probe classification, timeout, actual HTTP disconnect cancellation, and a real AI SDK `MockLanguageModelV4` multi-step loop proving step limits and aggregate usage.
