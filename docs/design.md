# How the runtime is built

The contract with Core and the reasons behind most choices are in Core's
handout, `docs/agent-runtime.md` in AIShiteru-Core (cited here as §n). This
document says how this repository meets it: what each package does, the
algorithms that matter, and the defaults. Where this code departs from the
handout, it says so and why.

## 1. The shape

One binary, `aishie-runtime`:

```
aishie-runtime run        the worker: pollers and answer loops, and /healthz, /metrics, /status
aishie-runtime check      validate the configuration; with --live, connect each agent and show its seats
aishie-runtime migrate    the store's schema (Postgres)
aishie-runtime catalogue  fetch Core's GET /v1/tools, print its hash, compare it with a snapshot
aishie-runtime version
```

The hosted UI of M2 (SSO, connecting by token, the secret store under KMS,
the owner's page) is not here yet. Agents are configured from YAML (§4) and
secrets come from the environment or files, as M1 has it; `/status` is the
owner's page's data, read-only.

```
cmd/aishie-runtime/     the binary
internal/
  core          Core's client: MCP (JSON-RPC over HTTP, written here) and REST transports,
                retries, the typed calls, the catalogue and its hash
  ratelimit     one token bucket per agent, with priorities (answers > polling > events)
  llm           the internal format (§3.1), adapter interface, provider detection and defaults
  llm/openaichat, llm/anthropic, llm/gemini, llm/openairesponses, llm/bedrock
                one adapter per model API (§3.10)
  llm/providers builds an adapter from configuration
  llm/scripted  an in-process adapter that plays a script, for tests
  llm/fakellm   an OpenAI Chat server that plays a script, for end-to-end tests
  toolschema    the sanitiser (§3.8): bind, common transform, dialects, reverse map, validation
  toolset       which tools a seat's model is offered (§4), and running the model's calls
  config        the YAML, its defaults and precedence, validation
  secrets       secret://, env://, file:// references
  pricing       the versioned price table, and cost
  redact        a log handler that removes tokens and keys
  safety        what the model wrote, made safe to post: links, images, length
  prompt        the system prompt and the history the model is given
  store         the runtime's state (interfaces); memstore, pgstore, storetest
  worker        the supervisor, each agent's pollers, the answer loop, quotas
  httpserver    /healthz, /metrics and /status
  metrics       Prometheus metrics (§8.1)
  fakecore      a fake Core with the same MCP surface and envelope, scriptable (§8.2)
e2e/            against a real Core, pinned in .github/core-image
```

## 2. Talking to Core

### 2.1 Transport

Core is stateless streamable HTTP with JSON responses (§1.2). The official Go
SDK's client folds a 401 and a 429 into generic errors and retries some on
its own; the runtime needs to tell them apart (a 401 stops the agent, a 429
has a `Retry-After`), so `core.MCPCaller` is a small JSON-RPC client written
here. It sends `initialize` once per agent with the pinned revision
(`2025-11-25`), then `notifications/initialized` (no id), and every call is
`tools/call` with `MCP-Protocol-Version`, `Authorization: Bearer`,
`Content-Type: application/json` and `Accept: application/json,
text/event-stream`. A `text/event-stream` answer is read too (its first
`data:` message), though Core sends none. The SDK is still used: the fake Core
is an SDK server set up as Core sets up its own, so the client is tested
against the same library Core runs.

`core.RESTCaller` is the other transport: the catalogue's `method` and
`path`, path parameters filled from the arguments, a GET's other arguments in
the query string, a POST's in the body with the key in `Idempotency-Key`.

Answers from either:

| Core says | The caller returns |
|---|---|
| a JSON-RPC result | the envelope from `structuredContent`, else parsed from `content[0].text` |
| HTTP 401 | `ErrUnauthenticated`: the supervisor stops the agent, state `unauthorized` |
| HTTP 429 | `*RateLimitedError{RetryAfter}` from the header, else the body's `details.retry_after_seconds` |
| HTTP 5xx, a network error, a timeout | `*TransientError` |
| a JSON-RPC error, anything unreadable | `*ProtocolError` |

`core.Retrying` sends the same bytes again on a `*TransientError` and on an
envelope with code `internal`, backing off 1, 2, 4 … 60 s with full jitter,
for as long as the context allows; on a 429 it sleeps `Retry-After` plus
jitter and tells the agent's limiter, which halves polling for five minutes.
The rate limiter (`ratelimit`) sits under it: every call waits for a token.

### 2.2 The rate limit

One bucket per agent, at 90 % of Core's allowance (540 a minute, burst 90 at
the defaults). Waiting calls are served by priority: answers, then polling,
then events and seats (`core.PriorityOf`). Polling also has a share of its
own (`max_rate_share`, 30 %): each seat's inbox interval is at least
`courses × 60 / (share × rate − event and seat calls a minute)` seconds
(§7.3).

### 2.3 The catalogue

`GET /v1/tools` (no token) is fetched at start and its hash kept: the sha256
of its canonical JSON. `core.Catalogue` holds each tool's MCP name, kind and
input schema. The permission gates of §4 are kept by hand in `toolset`,
checked at start against the catalogue: a gated tool missing, or no longer a
read, is refused. CI compares the pinned Core's catalogue with
`internal/core/testdata/catalogue.json`.

## 3. Models

`llm.Request` and `llm.Response` are §3.1's internal format. An adapter owns
its translation both ways and uses `net/http` for transport, not the
providers' SDKs: the transport is one POST of JSON, and owning it keeps raw
usage and raw stop reasons verbatim and the golden tests byte-exact. (§3.10
suggests SDKs for transport; the one place a library earns its keep, AWS's
SigV4 and credential chain, is used for Bedrock.)

Rules every adapter keeps (§3.1): arguments are a JSON object inside; a call
without an id gets `call_{n}`; any tool call means `stop: tool_calls`;
reasoning goes back only to its maker (`Part.Maker`, adapter + endpoint +
model); stop reasons map as §3.4; usage as §3.5, with `raw` kept.

`llm.Defaults(adapter, provider)` gives each provider's capabilities and
schema dialect (§3.3, §3.8); where the handout marks something unverified,
the default is the one that cannot break a call. ForceAnswer (the last turn
of a spent budget) sends `tool_choice: none` where `ToolChoiceNone`, leaves
the tools out otherwise, and where the API refuses tool history without
tools (`ToolsWithHistory`, Bedrock) flattens that history into text first.

Provider errors are `*llm.Error` with a kind; `Retryable()` kinds are retried
with backoff inside the wall clock, then the fallback model if there is one.

## 4. Tools

`toolset.Build(catalogue, seat perms, tools config, dialect)` is §4's
formula: the tool's gate is allowed by the seat's perms, it is in `allow`,
not in `deny`, not in the built-in deny list (§6.1), and a read. Unknown
gates offer nothing. Beside §6.1's list, `event_list` and `action_list_mine`
are never offered, though §4 gives them gates: the runtime reads them
itself, and `action_list_mine` returns the answers the agent wrote in other
people's conversations, which a worker answering one conversation must not
see. `deny` entries ending in `*` cover every tool they begin. The model sees each tool through `toolschema`: bound
arguments removed (`course_id`, `idempotency_key`), the common transform,
the adapter's dialect; cached per catalogue hash and dialect.

Running a call (`toolset.Set.Run`): the name must be offered; the arguments
must parse; `toolschema.Reverse` drops nulls the original schema does not
allow and puts the bound arguments back (`course_id` is always the
conversation's course, whatever the model wrote); the result is validated
against Core's own schema; then Core is called. Any failure before Core is
an `is_error` result the model can correct itself from. Results are Core's
envelope as JSON, cut at 32 KB keeping `status` and `error` whole.
`document_get`'s `download_url` never reaches the model: the runtime fetches
the file (at most 10 MB) and gives it as a file part where the adapter takes
files, as text when it is text, and as a sentence saying it could not be
read otherwise.

Calls in one turn run at most `max_parallel_tools` at once; results go back
in call order.

## 5. The worker

### 5.1 Supervision

`worker.Supervisor` loads the configuration and runs every agent it can
lease (`agent:{id}` in the store, renewed every 10 s, lasting 30 s). A dead
worker's agents are taken up by another within 30 s; two workers never run
one agent's pollers at once, so one process's token bucket is the agent's
whole spend. A renewal gets min(5 s, a third of the lease) to finish; one
that fails or does not finish stops the agent at once. On shutdown or
removal the agent is recorded `stopped` before its lease is let go, so a
takeover (`lease_takeovers_total`) counts only a lease that lapsed on a
worker that was still running it. Within one worker, one Core actor is one
agent: a second agent configured with the same token goes to state `error`,
naming the first. `SIGHUP` reloads the configuration and the price table:
agents added, removed, paused or changed are started, stopped or restarted.
A configuration with no agent is valid: the supervisor runs and waits, and
`run` (at start and on every reload) and `check` say so, so that a server
can be deployed before its first agent.

An agent (`worker.Agent`) starts with `me_get` (the token works), the
catalogue, and `me_memberships`. It reads memberships again every
`memberships_s`, and at once after a `forbidden`, `not_found` or `denied`.
Each seat that answers (`core.Membership.Answers()`, and the course not
disabled in the configuration) gets a `Seat`: its inbox poller, its events
poller, its toolset. A seat that leaves `me_memberships` is stopped, recorded
gone, and its memory purged `retention_days_after_removal` later. A seat
still listed that stops answering (paused, denied, the course archived or
disabled here) is stopped but not gone, and keeps its memory. Whether a
seat is a tutor or a delegate, and so which built-in prompt it gets, is read
at each answer from `answers_course` as `me_memberships` last showed it.

A 401 anywhere stops the agent (state `unauthorized`). So does an MCP
envelope with status `error` and code `unauthenticated`, which the real Core
sends where REST answers 401 when the token's actor no longer exists. A
reload starts an unauthorized or failed agent again even when its
configuration has not changed, because a new token goes into the same
secret file (`docs/deploying.md`). Tokens and model keys are read when an
agent starts, so a rotated one takes effect at the next start. A paused
agent makes no calls at all.

### 5.2 Polling

Per seat (§7.2):

- **Inbox**: every `inbox_hot_s` for `hot_window_s` after activity (an
  answer posted, an event of the opener writing), else `inbox_idle_s`,
  growing ×1.5 per empty poll to `inbox_max_s`; every interval jittered by
  `±jitter`, and never below the rate share's floor; the first poll of each
  seat spread over the idle interval. After a 429, the inbox, events and
  seats intervals all double for five minutes. A poll counts as empty only
  when it finds nothing new: rows waiting for a slot do not slow the next.
- **Events**: every `events_s`, and after a `proposed` answer at once, then
  after 5, 15 and 45 s. The cursor (`next_seq`) is kept in the store per
  seat, and moves past a page only when every event on it was recorded; a
  store that fails on one has the page read again. The real Core does not
  move `next_seq` over events the actor cannot see, so a page with no
  events ends the round.
- **Seats**: every `memberships_s`; an agent with no seat to poll reads them
  every `inbox_max_s` instead, so that Core still shows it present and a
  restored seat is noticed soon.

Each inbox row not already being answered, not held back (below), goes to
the scheduler, which runs at most `max_concurrent` answers per agent and
`max_concurrent_per_course` per course; the rest wait for the next poll.

### 5.3 One answer

For an inbox row (conversation X, question M, opener P):

1. **Lease** `conv:{agent}:{X}` for the wall clock plus 30 s; if another
   worker holds it, leave the row.
2. **Unsettled attempt?** An attempt at M found `sending` (a crash, a
   timeout) is sent again with its stored bytes before anything else.
3. **Attempt number**: one more than the attempts at M so far, all settled
   without posting. Past `max_attempts`: close X (`close:{X}`, the configured
   reason) or skip it until tomorrow, per `on_attempts_exhausted`.
4. **Quota** (§5.3): the asker's day (course, P), the agent's day, the
   tenant's day, each in answers and dollars; dollars checked against the
   p95 of the agent's recent answers. A dollar quota already spent is out;
   a scope that has spent nothing today is never refused on the p95 alone,
   or one dear answer would lock every asker out for good. Answers count
   against quotas when Core took them (executed or proposed); dollars count
   every model call. Out of quota: the canned notice under the answer's key,
   with no model call (`on_quota_exhausted: canned`), or skip until tomorrow
   (`silent`).
5. **Read** X: `conversation_messages` (the newest `history_messages`). If
   the opener's newest message is no longer M, answer that one instead.
6. **Prompt**: the system prompt (§6 below), the seat's facts, X's memory,
   and the history as turns: the opener's messages as `user`, the agent's as
   `assistant`, retracted ones as `[message retracted]`.
7. **Loop** (§7.1) with the seat's toolset, bounded by `per_answer`. Stop
   `end` gives the body. `max_tokens` with partial text is tried once more
   with twice the cap; `content_filter` and `refusal` give
   `on_refusal_text`; `context_overflow` halves the history and tries once
   more; `tool_error` retries the turn once. A spent budget takes a last
   turn with ForceAnswer, and gives `on_budget_text` if that has no text.
   `turns` is a hard cap: the last call it allows is the forced one, and a
   provider's error is not a turn. The output-token budget forces the last
   turn as soon as what is left cannot hold a whole one, and caps it at what
   is left; tool calls past their budget get an error result and never reach
   Core. A last turn forced by the wall clock gets min(wall clock / 6, 15 s)
   more; the claim's Core calls stop at the wall clock plus 25 s, inside the
   lease. A provider that cannot be reached is tried up to three times
   with backoff within the wall clock (honouring `Retry-After`), then the
   fallback model, which an auth or bad-request error also moves to. While a
   fallback remains, a call gets two thirds of the time left, and one that
   times out moves to the fallback at once, so that a provider that hangs
   leaves its fallback time to answer. If all fail, nothing is posted and
   X is held back for a minute, doubling to ten; after five such failures
   on M (counted in memory), `on_budget_text` is posted.
8. **Safety** (`safety.Body`, §7 below): links and images whose URLs carry
   context stripped, cut to `max_body_chars` on a paragraph or sentence.
9. **Post**, written ahead: the attempt is stored (`sending`, the exact
   bytes) before `conversation_answer`, and finished with what came back.
10. **Outcome** (§2.4, `worker.Classify`):

| Envelope | Done |
|---|---|
| `executed` (any review state) | record; course hot; memory note `answered` |
| `proposed` | keep the action id (state `proposed`); events polled now, +5, +15, +45 s |
| `denied` | hold the seat until `me_memberships` changes; agent detail says so |
| `failed conflict moved_on` | back to 5 for `details.latest_opener_message_id` (at most 3 times per claim) |
| `failed conflict already_answered`, `answer_pending` | leave it |
| `failed conflict closed` | drop it |
| `failed forbidden not_addressable` | drop it; read memberships again |
| `failed invalid_argument` | if safety had cut or stripped the body, written again once, shorter and without links, under the next attempt; else failed |
| `error idempotency_conflict` | never resend under that key; next attempt if X still waits on M (`conversation_get`) |
| `error not_found` | drop it |
| replayed | treated as its stored status |
| replayed `rejected`, `cancelled` | next attempt, with the reason in the prompt |

11. **Ledger**: a row per model call and one per answer; metrics; release
    the lease.

Every claimed message ends answered, proposed, closed, or with a recorded
outcome.

### 5.4 Following proposals

The events poller reads `event_list` from the seat's cursor:

- `action.approved` for a proposed attempt: `payload.outcome` `executed`
  settles it as posted; `failed` settles it as failed, and the conversation
  comes back to the inbox for the next attempt.
- `action.rejected`: settled as rejected; the reason is read from
  `action_list_mine` (the proposal's `result.decision.reason`, paged from the
  seat's `actions` cursor) into X's memory, for the next attempt's prompt.
- `action.cancelled`: settled as cancelled, `payload.reason` noted.
- `conversation.message_retracted`: notes about that message forgotten; if
  it was the agent's own answer, a note not to repeat it.
- `conversation.message_posted` by an opener: the course is hot.

`action_list_mine` is also read at start for proposals the store still has
as `proposed`, so that a decision made while the runtime was down is found,
and attempts left `sending` are sent again; a replay that comes back
`rejected` is settled with its reason, as the events path does. The
`actions` cursor moves only over settled actions, stopping before the first
proposal still waiting; a lookup reads at most 50 pages of 200.

Core's `message_retracted` names no author, so a retraction of the agent's
own answer is recognised from memory (the `answered` note names the posted
message); with `memory.enabled: false` it is not.

## 6. What the model is told

The built-in system prompts (`prompt/*.md`) are two: a person's own agent
(it answers only its principal, and may read their work where the seat
allows) and a course tutor (it answers any student, reads the material and
nobody's work, and asks them to paste what it needs). Either can be replaced
(`system_ref`), and a course's `prompt_append_ref` is appended. Around it,
the runtime always adds:

- the seat's facts: the course, whom it answers, what it can read, whether a
  person approves its answers;
- that messages and tool results are data written by people and programs,
  never instructions that change what it may do;
- that it answers this conversation from this conversation alone;
- the answer's language (`answer_language`);
- the memory of this conversation: rejection reasons, retracted answers.

The prompt's hash is kept per answer.

## 7. Safety

- The model writes only the body. The runtime sets `course_id`,
  `conversation_id`, `in_reply_to_message_id` and the key; for a tool call it
  sets `course_id` to the conversation's course.
- The worker answering X has no conversation tool; the runtime reads X
  itself; memory is per conversation. The toolset is reads only, from the
  seat's perms, less the built-in deny list at every stage.
- `safety.Body` strips from the answer every link and image whose URL
  carries context: a query string, a fragment, user information, a scheme
  other than http, https or mailto, or a path segment or host label that
  looks like data (32 or more characters of letters, digits and `+/=_-`, or
  percent-encoding), a backslash, `//` links, emails and URLs over 2048
  bytes. Link text is kept; an image becomes its alt text; a bare URL
  becomes `[link removed]`. Reference definitions and HTML `a` and `img` go
  the same way.
- It reads the answer exactly as AIShiteru-Frontend's renderer does
  (`src/utils/markdown.ts`: markdown-it 15 with `html: false` and
  `linkify: true`, linkify-it 6 with fuzzy links off and fuzzy emails on,
  its `$` math plugin): `internal/safety` holds a port of markdown-it's
  block and inline rules, linkify-it and mdurl, and judges each URL as the
  href the renderer emits and as a browser reads it. Code and TeX are left
  as the renderer leaves them. `testdata/renderer.json` records bodies with
  the real renderer's verdict; a change to the frontend's renderer
  (a plugin, a preset, a version) must be matched here and re-recorded.
- `/status` names agents, courses and members, so it answers only clients on
  the loopback interface; `HTTP_ADDR` defaults to `127.0.0.1:9090`.
- `redact` removes `ais_…`, `aisinv_…`, `sk-…`, `AIza…`, AWS access keys
  and `LOG_REDACT_EXTRA` from every log line. Logs hold ids, counts and
  codes, never message text.

## 8. The store

| Table | Holds |
|---|---|
| `lease` | name, holder, expires_at |
| `attempt` | (agent, key) → the exact bytes, state, action id, posted message id |
| `cursor` | (agent, member, kind) → value |
| `note` | (agent, member, conversation) → kind, text, message id |
| `seat` | (agent, member) → course, seen_at, gone_at |
| `llm_call`, `answer` | the ledger: ids and numbers |
| `agent_state` | the owner's page's state |

`memstore` keeps the same in memory, for one worker and for tests: it loses
attempts and memory on restart, so a restarted worker may find its keys
taken (`idempotency_conflict`) and move to the next attempt number. Use
Postgres (`DATABASE_URL`) for anything that matters.

## 9. Defaults

The built-in defaults are §4's example: MCP at `2025-11-25`; tools derived,
the read tools of §2.3 allowed, four in parallel; three attempts, then
close; the canned notice when out of quota; 19,000 characters; the newest 30
messages; eight answers at once per agent, four per course; per answer 8
turns, 12 tool calls, 150,000 input and 4,000 output tokens, 90 s; no daily
quotas unless set (a school key requires per-agent and per-asker ones);
polling 2 s hot for 120 s, 10 s idle to 30 s, events 45 s, seats 300 s,
±25 %, 30 % of 600 a minute; memory on, purged 30 days after a seat goes.

## 10. Tests

- `fakecore`: the MCP surface and envelope of Core, scriptable: questions,
  follow-ups written during generation, levels changed, seats paused or
  removed, proposals approved, rejected or expired, retractions, 429s and
  401s. `internal/fakecore/testdata/fixtures` are envelopes recorded from
  the pinned Core for every row of §2.4 and more (`make record-fixtures`
  against a live Core); a conformance test holds the fake to them, and
  Core's own client is tested live against the real one.
- Adapters: golden translations both ways in `testdata/`, every stop reason
  and usage field; `LIVE=1` runs them against the real providers whose keys
  are set, with one request declaring every tool at 16 output tokens.
- `toolschema`: every tool of the pinned catalogue through every dialect and
  back through Core's schema.
- `storetest`: one suite, run against memstore and against Postgres
  (`TEST_DATABASE_URL`).
- `worker`: the fake Core and the scripted model: every row of §5.3's table,
  moved on, duplicates across two workers, denied, 401, 429, quotas,
  budgets, proposals followed, retractions; no token in any log line; the
  safety evaluations (injected instructions to call other tools, to answer
  about other students, to post links carrying data).
- `e2e`: the pinned Core (`scripts/ci-core.sh`), agents seated over REST, the
  runtime with the scripted OpenAI Chat server behind the real `openai_chat`
  adapter: a student's own agent answers within the latency target and a
  course tutor keeps askers apart; moved on and duplicates across two
  workers are safe; a seat set to `denied` stops polling and answers again
  when restored; a proposal approved is recorded, and a rejection's reason
  reaches the next attempt; no token or key in any log, before or after
  redaction; the binary's `catalogue --check` and `check --live`.
