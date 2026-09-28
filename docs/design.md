# How the runtime is built

The contract with Core and the reasons behind most choices are in Core's
handout, `docs/agent-runtime.md` in AIShiteru-Core (cited here as §n). This
document says how this repository meets it: what each package does, the
algorithms that matter, and the defaults. Where this code departs from the
handout, it says so and why.

## 1. The shape

One binary, `aishie-runtime`:

```
aishie-runtime run        the worker: pollers and answer loops, and /healthz, /metrics, /status;
                          with API_ADDR, the JSON API for the front end on a listener of its own
aishie-runtime check      validate the configuration; with --live, connect each agent and show its seats
aishie-runtime migrate    the store's schema (Postgres)
aishie-runtime keys       the sealed secrets: check that each opens, or rewrap them under the current key
aishie-runtime catalogue  fetch Core's GET /v1/tools, print its hash, compare it with a snapshot
aishie-runtime version
```

Agents are configured from YAML (§4), with secrets from the environment or
files, as M1 has it, and, with the store in PostgreSQL, from the registry of
hosted agents that people connect from AIShiteru-Frontend (§11): the secret
store seals their tokens and their owners' keys in the runtime's database,
and the registry runs them beside the YAML agents. The JSON API the front
end calls (§11.4) listens apart, on `API_ADDR`. `/status` is the
operator's view, read-only, and never served through the API's listener
or a proxy.

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
  secrets       secret://, env://, file:// and sealed:// references
  vault         envelope encryption of the secrets kept in the store: a data key per secret,
                wrapped by the key KMS_KEY_ID names (§11.1)
  registry      hosted agents: each row made the agent document YAML would hold, loaded on its
                own, merged with YAML; the watcher that reloads on the registry's changes (§11.2)
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

The configuration is YAML ∪ registry (§11.2), rebuilt on `SIGHUP`, at each
notification of the registry (`LISTEN aishie_registry`), and when a poll
every 30 s finds the registry's revision moved on. A rebuild from the
registry goes through `Supervisor.Update`, which restarts only the agents
whose configuration changed: an agent stopped until a reload (Core refused
its token) stays stopped through another agent's change, and a hosted
agent's new token, being a new secret, is a change of its own, which starts
it again. `SIGHUP` goes through `Reload`, which starts every such agent
again, since a new token may be in the same file. A hosted agent that does
not pass is not run, and is shown in state `error` with every problem; it
keeps none of the others from running.

A hosted agent's token must be its own: `me_get` must name the Core actor
its row does, and that actor must be an agent (a person's own token would
have the runtime act as the person, in every seat of theirs), or it is
stopped in state `error` until it changes; `check --live` fails it the same
way, before reading anything more with the token. And the
operator's configuration wins a Core actor: a hosted agent on the actor of a
YAML agent this worker runs is stopped in state `error`, whichever started
first, until a reload.

A hosted agent runs only for its owner (the product owner's D5): at each
start, `me_get`'s `owner_actor_id` (Core's C1) must be the owner its row
names, the person who connected it. When Core names another owner (the
agent was given to someone else, or connected by someone who held its
token without owning it) or none (its owner was taken away), it is
stopped in state `owner_changed`, whose detail says that its owner in Core
is no longer the person who connected it here and that its owner must
connect it again, naming no one; it stays stopped, making no call, through
other agents' changes, until its row changes (its owner connecting it
again) or a reload, as an unauthorized agent does. A Core from before C1
says nothing of owners, which `me_get`'s answer alone cannot tell from an
agent nobody owns; its catalogue can, since only a Core that names owners
describes `owner_actor_id`. There the owner cannot be checked, and holding
the token is not taken as proof of it: the agent is stopped in state
`error`, saying that Core must be upgraded (`worker.HostedOwnerProblem`).
`check --live` fails an agent the same way, naming the state `run` would
give it. When the check passes on a row not yet marked `owner_verified`
(one stored while holding the token was the proof), the worker marks it,
with the store's only write of a row, `UpdateHostedAgent`, at the version
it read: the row's version and the registry's revision move on, and the
rebuild that follows restarts nothing, since whether an owner has been
verified changes nothing in how an agent runs. YAML agents are not
checked: their owner is whoever their operator says.

An agent (`worker.Agent`) starts with `me_get` (the token works), the
catalogue, and `me_memberships`. It reads memberships again every
`memberships_s`, and at once after a `forbidden`, `not_found` or `denied`,
and records each seat as it then is (course, status, `answers_course`,
principal, perms), so that its seats can be shown without its token.
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
(`system_ref`, or the text itself, `system_text`, at most 20,000
characters), and a course's `prompt_append_ref` (or `prompt_append_text`,
at most 4,000) is appended. A hosted agent's prompts are always the text. Around it,
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
- `redact` removes `ais_…`, `aisinv_…`, `sk-…`, `AIza…`, AWS access keys,
  JSON Web Tokens (`eyJ….….…`, the assertions of §11) and
  `LOG_REDACT_EXTRA` from every log line. Logs hold ids, counts and codes,
  never message text.

## 8. The store

| Table | Holds |
|---|---|
| `lease` | name, holder, expires_at |
| `attempt` | (agent, key) → the exact bytes, state, action id, posted message id |
| `cursor` | (agent, member, kind) → value |
| `note` | (agent, member, conversation) → kind, text, message id |
| `seat` | (agent, member) → course, seen_at, gone_at, and the seat as `me_memberships` last showed it: course code, title and section, status, `answers_course`, principal, perms |
| `llm_call`, `answer` | the ledger: ids and numbers |
| `agent_state` | the owner's page's state |
| `secret` | sealed secrets (§11.1): id, tenant, kind, the key's id, the wrapped data key, nonce, ciphertext, hint |
| `person` | who has used the API: Core actor, name, platform role, last seen |
| `hosted_agent` | the registry (§11.2): id `agt_…`, Core actor (unique), owner and whether Core said so, tenant, name, token and own key (secrets, with hints), paused, settings (jsonb), version |
| `hosted_course` | (agent, course) → settings (jsonb), who wrote them, when |
| `registry_rev` | one row: the revision every write to `hosted_agent` or `hosted_course` moves on, by trigger, with `NOTIFY aishie_registry` |
| `audit` | the API's audit (§11.4): when, who, with which of Core's sessions, from where, what, to what, the outcome, and a detail of ids, hints, providers, models and results; kept 400 days |

Beside the sums quotas are checked against (`Spend`), two reports read the
ledger for people, ids and numbers only: `Usage(agent, since, until)`, a
row per UTC day and course (billable answers, every answer by outcome,
model calls, tokens and cost), and `AskerUsage(agent, course, since,
until)`, the same per asker, counts only, never what anyone wrote. The API
shows an agent's seats from the seat rows, and never needs its token to.

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
- `vault`: a secret sealed and opened; every field and byte of it tampered
  with, and moved to another id, tenant or kind, fails to open; a key
  rotated (added, rewrapped, retired); keyrings that cannot be used are
  refused without repeating what `KMS_KEY_ID` says. The worker starts an
  agent from `sealed://` references against the fake Core, and
  `keys check|rewrap` and `check --live` are run on Postgres.
- `registry`: documents made from rows, and every setting a hosted agent may
  not have; YAML ∪ registry with a YAML agent winning an id, one bad row
  beside good ones, no Core; the official endpoints; the watcher on Postgres
  by notification, by poll, and listening again after its connection is
  killed. `storetest` holds both stores to the seat snapshot, and the
  reports by day, course and asker, at the UTC day's edges and the span's.
  The worker runs hosted agents from their sealed secrets beside
  YAML ones, pauses them, keeps an unauthorized one stopped through others'
  changes and starts it again on its new token, and lets YAML win a Core
  actor whichever started first; the binary picks up an agent connected
  while it runs, told by the notification; and the end to end connects
  Yuki's agent to a runtime on Postgres, which answers her against the real
  Core, with no token, key or the key that seals them in its logs, in any
  table of its database, or in its status.
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

## 11. Hosted agents

M2 lets people connect their own agents from AIShiteru-Frontend instead of
an operator writing YAML. The runtime's side is built in steps: the secret
store (§11.1), the registry of hosted agents that runs them beside the YAML
agents (§11.2), and a versioned JSON API for the front end (§11.4).

### 11.1 The secret store

`internal/vault` is envelope encryption:

- Each secret, an agent's Core token or a model key, has a data key (DEK)
  of its own, 256 random bits, and is encrypted under it with AES-256-GCM.
  The additional data is `"aishie/secret/v1" ‖ secret_id ‖ tenant_id ‖
  kind`, each field a 4-byte length and its bytes, so that fields cannot
  run together: a ciphertext moved to another row, tenant or kind does not
  open.
- The DEK is wrapped by the key-encryption key (KEK), bound to the same
  additional data; the row keeps the KEK's id beside it. The interface is
  `Wrap(ctx, dek, aad)` and `Unwrap(ctx, kekID, wrapped, aad)`.
- `KMS_KEY_ID` chooses the KEK. `local:<dir>/<name>` is a 32-byte key,
  base64, in the file `<dir>/<name>` (`/secrets/kek/v1` on a server,
  `docs/deploying.md`); every other file in `<dir>` is kept as a keyring, so
  that secrets an older key wrapped still open. A key's id is
  `local:<name>`, whatever the mount. `awskms:` and `vault:` are refused as
  not built yet; with AWS KMS, the additional data's fields become the
  encryption context, and a process that only seals needs `Encrypt` and
  never `Decrypt`.
- The `secret` table (migration 0002) holds id (`sec_…`), tenant, kind
  (`core_token` or `model_key`), the KEK's id, the wrapped DEK, the nonce,
  the ciphertext, a hint, who gave it and when. No column holds plaintext.
  The hint is all of a secret that is ever shown: for a Core token, `ais_`
  and its 12-character public prefix (Core's `credential_list` names the
  token by it); for a model key, its provider's prefix and last four
  characters (`sk-…3f9a`), fewer for a short key.
- A secret is referred to as `sealed://<secret id>`. `secrets.Resolver`
  opens it through `vault.Opener`, reading the row from the store, only
  where the worker resolves secrets (`Agent.start` for the token,
  `buildModel` for keys, for the life of the agent instance) and in
  `check --live`. Without `KMS_KEY_ID` a sealed reference is refused, saying
  so. The resolver also refuses a `secret://` or `file://` reference that
  reaches the keyring's directory, which lies in `SECRETS_DIR`, links
  followed, or, once the file is open, that is one of its keys by any other
  path (a key file linked from elsewhere, a hard link, the directory mounted
  twice): a KEK sent to a provider as an API key would be every secret.
- `aishie-runtime keys check` opens every secret with the keyring and says
  which key wraps how many; `keys rewrap` wraps every DEK an older key
  wraps under the current one, a row at a time (the ciphertext is not
  touched, and the DEK is proved against it first), so that a key is
  retired by adding the next, pointing `KMS_KEY_ID` at it, rewrapping, and
  removing the old file (`docs/deploying.md`). Neither prints a secret.
- Deleting a secret destroys its row. Its ciphertext stays in the
  database's backups until they rotate out, which is why the keyring is
  kept apart from them.

### 11.2 The registry of hosted agents

A hosted agent is a row of `hosted_agent` (migration 0003), with its
courses' settings in `hosted_course`: what the API writes, from the person
who connects the agent. `internal/registry` turns each row into the agent
document a YAML file would hold, and runs it beside the YAML agents:

- **The document.** The row's `settings` (the agent document of §4, as
  JSON) are the document, with what the registry sets itself: `id`,
  `display_name`, `tenant_id` and `paused` from the row; `core` as
  `{base_url: CORE_BASE_URL, token_ref: sealed://<token_secret_id>}`; and
  the owner's key, `sealed://<key_secret_id>`, on each model section whose
  key source, as written or inherited from its parent, is `own`, with that
  key source written out (merged over `runtime.defaults`, a fallback that
  names none would take the defaults' fallback's first, and be paid for as
  the school's). Each course's row is its
  `courses[course_id]`. Settings that set any of those themselves, or refer
  to any file or secret (a key ending in `_ref`, anywhere), are refused: a
  hosted agent reads nothing but its own sealed secrets.
- **The same path as YAML.** `config.LoadDocuments` reads each document as
  `config.Load` reads a file's (strictly: every key known, every value of
  its type), over the built-in defaults and `runtime.defaults`, and
  validates it with the runtime's tenants, model lists and
  `CORE_BASE_URL_ALLOWLIST`, but each on its own: one that does not pass is
  rejected with every problem, where `Load` stops at the first file's.
- **What a hosted agent may call.** Every model it calls, in every course,
  is on the owner's key source and has the owner's key and no other, as
  merged and decoded, never one it would inherit from the
  runtime's defaults (a fallback it does not set is none, not the
  defaults'); none is called with the runtime's own credentials (Bedrock
  without a key would sign with the host's) or at a server that takes no
  key; `base_url` is empty (the adapter's own) or an official provider's
  endpoint (§3.9) over https, the provider's own host where it serves
  from a cloud's domain (DashScope's under `aliyuncs.com`, Bedrock's
  runtime under `amazonaws.com`, not a bucket or function anyone can
  name there); and it sends no extra headers (D9). The
  school's key is not offered to hosted agents yet: that waits on the
  school's model offers and the owners' quotas (below).
- **YAML ∪ registry.** `registry.Build` is the YAML configuration, then
  every hosted agent that passes; the rest are in `Config.Rejected`, which
  the supervisor shows in state `error`. A hosted agent whose id is a YAML
  agent's loses to it, as does one on a YAML agent's Core actor (§5.1).
  Without `CORE_BASE_URL`, no hosted agent runs, and each says why. The
  registry is on only with the store in PostgreSQL: with memstore there is
  nowhere to keep it, and `run` says it is off.
- **Reloading.** Every statement that writes `hosted_agent` or
  `hosted_course` moves `registry_rev` on and notifies `aishie_registry`,
  by trigger, so a write made by hand counts too. `registry.Watcher`
  listens on a pgx connection of its own, not the pool's, connecting again
  with a backoff when it is lost, and reading the registry once it listens,
  for what changed meanwhile; a poll of the revision every 30 s covers a
  notification lost. Every read of the registry, the poll's, a rebuild's,
  SIGHUP's and the start's, ends within 10 s, so that a database that does
  not answer (a lock held, a connection lost without a word) holds up
  neither the watcher nor SIGHUP, nor the signals after it. A registry
  that cannot be read leaves the configuration in force as it is.
- **The store's side.** Creating an agent stores the secrets it refers to
  in the same transaction; its token must be a `core_token` and its key a
  `model_key` of its tenant, and no other agent's. An update names the
  version it read (If-Match), and is refused at any other; its Core actor
  and tenant never change; a secret it no longer refers to (a token or key
  replaced) is destroyed with it. Deleting it destroys its courses and its
  secrets in one transaction. `registry_rev` moves on with none of the
  people's or the secrets' writes.
- **The key pool (D8).** The document's shape holds what the product owner
  decided for a student's own agent: `model` is the school's offer and
  `model.fallback` the owner's own-key model, whose key is the row's
  `key_secret_id`; the worker already falls back when the school's model
  cannot be reached. Switching when the owner's daily quota on the school's
  key is spent, and holding the question when there is no own key, come
  with the quotas; until the school's offers exist, a hosted agent on the
  school's key is refused.
- `check`, with `DATABASE_URL`, reads the registry as `run` does, lists
  the hosted agents it would run and those it would not, with why, and
  passes: they keep no other from running. A registry it cannot read (a
  schema older than the binary's, before a deploy's `migrate up`) is said,
  and passes too. `check --live` connects the hosted agents as well.

### 11.3 Where M2 departs from the handout

- **The UI lives in AIShiteru-Frontend** (the product owner's D1), not in a
  `runtime-web` of the runtime's (§8.3): the runtime will serve a versioned
  JSON API only, and no HTML. People will authenticate to it with a
  short-lived assertion Core mints for its signed-in person (Ed25519, the
  runtime as audience), not as an OIDC client of the institution (D2); the
  runtime never sees Core's session cookie, and `OIDC_*` is not a runtime
  setting.
- **A data key per secret, bound to its tenant** (D12), where §5.4 has a
  data key per tenant. The tenant is in every secret's additional data, so
  the isolation is the same; each secret can be destroyed on its own; and
  with a KMS, the API that seals what people give it can hold no right to
  read anything back.
- **The front end finds the runtime while it runs**, by `GET
  /runtime/api/v1/info` on its own origin, not by a build setting: the web
  image is built once for every environment.

### 11.4 The API for the front end

`internal/api` serves `/runtime/api/v1/` on `API_ADDR` (`127.0.0.1:9091`
over SSH, `:9091` in the compose stack), behind Caddy at `/runtime/api/*`
on Core's own origin with the `Cookie` header stripped, and nothing else:
`/healthz`, `/metrics` and `/status` stay on `HTTP_ADDR`, and `/status`
also refuses any request a proxy forwarded (`Forwarded`, `X-Forwarded-*`,
`X-Real-IP`), since a proxy on the same machine connects from loopback.
The API needs `CORE_BASE_URL`, `API_AUDIENCE`, `DATABASE_URL` and
`KMS_KEY_ID`, and `run` refuses `API_ADDR` without them.

- **Who is calling** (D2). The front end asks Core for an assertion of
  its signed-in person (`POST /v1/auth/assertion`, audience
  `API_AUDIENCE`) and sends it as a bearer token. `internal/webauth`
  checks it, in this order, with the standard library alone: at most 8 KB,
  a JWS of three base64url parts (else `assertion_malformed`); a header of
  `alg` EdDSA, `typ` JWT when given, a `kid`, and no `crit`, `jku`, `jwk`,
  `x5u` or `x5c`; the key its `kid` names; the signature; then `iss` equal
  to `CORE_BASE_URL` and `aud` to `API_AUDIENCE` byte for byte (a list is
  refused), `kind` human, `sub` a UUID, `jti` and `sid` there (else
  `assertion_invalid`); and, with 5 s of leeway, before `exp` and not
  before `nbf` (else `assertion_expired`), not issued in the future, and
  lasting at most 900 s, Core's longest `ASSERTION_TTL`. The keys are
  Core's `GET /v1/auth/keys`, fetched through the egress client (5 s, at
  most 64 KB, no redirect) and kept for Core's `max-age`, at most 300 s; a
  `kid` it lacks fetches them again, at most every 30 s, one fetch at a
  time; while Core cannot be reached the last set is used for an hour past
  its age, and with none the answer is 503 `keys_unavailable`. A pinned
  `CORE_ASSERTION_KEY` replaces the fetch, and an assertion must name it
  by its RFC 7638 thumbprint, as Core does. The assertion is never logged,
  stored or passed on; a refusal is counted
  (`aishie_api_auth_failures_total{reason}`) and logged at debug with its
  reason alone.
- **Who is an administrator** (D4): Core's `platform_role` root or admin,
  narrowed to `ADMIN_ACTOR_IDS` when that is set. The role is the
  assertion's own `platform_role` claim, which Core signs, having read the
  actor afresh when it made the assertion: the runtime holds no credential
  of the person's to ask Core with (D2), and needs none. A role taken away
  in Core reaches the runtime within the assertion's lifetime (five
  minutes by default, fifteen at most). No v1 route grants an
  administrator more than an owner; `GET /me` says whether they are one.
- **Every request**, in order: one log line and metrics
  (`aishie_api_requests_total{route,code}`,
  `aishie_api_request_seconds{route}`): method, route, status, reason,
  person and milliseconds, never the `Authorization` header, a body, a
  token, a key, a name or an email; a panic answered as 500; the headers
  of every answer (`Cache-Control: no-store`, but `public, max-age=60` for
  `/info`; `nosniff`; `Content-Security-Policy: default-src 'none';
  frame-ancestors 'none'`; `Referrer-Policy: no-referrer`;
  `Cross-Origin-Resource-Policy: same-origin`; no CORS); the client's
  address, the peer's or, from a proxy in `API_TRUSTED_PROXIES`, the last
  `X-Forwarded-For` hop that is not one; `http.CrossOriginProtection`; the
  route (404 `no_route`, 405 with `Allow`); limits, as token buckets of at
  most 10,000 keys each (per address, 120 a minute, bursts of 60, for
  requests without an assertion and those whose assertion was refused, and
  30 a minute for refusals alone; per person, 120 a minute, bursts of 40,
  and for the routes that take a token 10 a minute, bursts of 5, and
  `keys/test` 6 a minute, bursts of 3, and 100 a UTC day), answered 429
  with `Retry-After`; the assertion; a query (none is taken)
  and a body (JSON only, at most 64 KB, no key twice, no member the route
  does not take, nothing after the object; a route of no body takes an
  empty one or `{}`).
- **Refusals** are Core's envelope, `{"error": {"code", "message",
  "details": {"reason", …}}}`: Core's codes and HTTP statuses, with
  `version_mismatch`, `version_required` and `unavailable` (503, with
  `Retry-After: 5`), and a reason from a closed list, which the front end
  words. A 401 is always the assertion's, and carries `WWW-Authenticate:
  Bearer realm="aishie-runtime"`, with `error="invalid_token"` when one was
  sent.
- **The routes.** `GET /info`, which anyone may ask, cached a minute:
  `api: "aishie-runtime"`, `api_version: 1`, the version and commit, the
  audience to ask Core for, the issuer, and the features offered
  (connecting by token and the owner's own key when the API has a Core
  and a vault, as `run` always gives it; never the school's key yet).
  `GET /me`: the person's actor id and name, whether they are an
  administrator, and how many agents they host; it records the person
  (`person`), at most every five minutes. The rest are a hosted agent's
  life, each the owner's alone (another's agent is 404, never 403):
  - `POST /agents/inspect` and `POST /agents` take an agent's token and
    ask Core, with it, what it is (`internal/probe`): `me_get`, then
    `me_memberships`, refused in order when Core refuses the token or
    cannot be reached, when it is a person's, a suspended agent's,
    another agent's than the one meant, an agent without an owner (or a
    Core too old to say) or someone else's. Connecting seals the token
    in a new row (`needs_model`) and records the agent's seats; the same
    token again replays the row; another token of an agent hosted
    already is `already_hosted`; an agent Core has given the caller since
    an earlier owner connected it is taken over, the earlier row deleted
    and purged; one the operator's YAML runs is `operator_agent`. Both
    answers list the agent's other live tokens (`other_tokens`, with
    Core's `credential_list`) and whether one was used in the last 15
    minutes, for the front end to warn that an agent has one brain at a
    time.
  - `GET /agents` and `GET /agents/{id}`: the agent as its owner reads
    it, with its `version` as a strong ETag, its seats, the proposals
    waiting, today's answers and cost, its model and key hint, and a
    `status` the API works out from the row and the worker's state: paused,
    then no model (`needs_model`), then `starting` until the worker has
    written a state for the row's version, then the state's own, with the
    reason the worker wrote (`token_refused`, `settings_rejected`,
    `agent_suspended`, `owner_changed`, …).
  - `PATCH /agents/{id}`, by merge-patch and only with `If-Match` (428
    without, 412 at another version): the owner's model, from the
    provider offers of `GET /models`, and their key, sealed; a model on a
    key of another provider, a denied model, or a row the registry would
    not run (`registry.Check`, the same path as `Build`, for this one
    row) is refused before anything is written. `POST /keys/test` tries a
    key with one output token, and neither stores nor returns it.
  - `PUT /agents/{id}/token`: a new token of the same agent, sealed in
    place of the old, whose secret is destroyed with the write; the new
    token then revokes the old in Core (`credential_list`, then
    `credential_revoke` of that credential alone, D7). `POST …/pause` and
    `…/resume` set the row's flag.
  - `DELETE /agents/{id}`: the stored token opened, the one secret the
    API ever opens, to revoke itself in Core (unless
    `revoke_token=false`); then the row, its courses and its secrets
    destroyed in one transaction, and the agent's notes, attempts,
    cursors, seats, state and leases purged, its ledger kept. An agent
    suspended in Core cannot revoke its own tokens: the answer says so,
    and its owner revokes them in AIShie.
- **Hosted agents' models** (D9) are called at the providers' own
  endpoints alone, which the API makes from the provider, an endpoint
  choice, an Azure resource or an AWS region (patterns with no dots),
  and the registry checks again. The worker calls them through
  `internal/netguard`: the dialer resolves the host itself and dials
  only public addresses (never loopback, private, link-local and the
  metadata address, CGNAT, or the other reserved ranges, IPv4-mapped
  forms included), checks the address again as it connects, and no
  redirect is followed. Behind `EGRESS_PROXY` only the proxy is dialed,
  and the proxy must refuse the same.
- **The audit** (D11): `Server.Audit` records an event in `audit`
  (migration 0005) after the change it is about has committed: who, their
  session, their address, the action, its target, the outcome and a
  detail of ids and hints, never a secret, a name or text anyone wrote. The
  store has no transaction that spans a registry write and an audit row,
  so an event that cannot be recorded is logged at error and counted
  (`aishie_api_audit_failures_total`), and fails nothing. Refused
  assertions are counted, not audited. Housekeeping destroys events older
  than 400 days.
