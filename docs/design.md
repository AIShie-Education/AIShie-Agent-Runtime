# How the runtime is built

The contract with Core and the reasons behind most choices are in Core's
handout, `docs/agent-runtime.md` in AIShie-Core (cited here as §n). This
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
hosted agents that people connect from AIShie-Frontend (§11): the secret
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

A call that waits for news (`wait_s`, §5.2) is marked so (`core.WithWait`),
and differs in three ways. Its HTTP timeout is its wait plus 15 s
(`core.WaitMargin`), over MCP and REST alike: a copy of the client, sharing
its transport, where the client's own timeout is shorter; every other call
keeps the client's (`core.DefaultTimeout`, 30 s, unless the operator's
client has its own). It takes one token, like any call, and holds none
while it waits: the token is spent before the call is made. And `Retrying`
sends it again after a 429 but not after a transient failure or an
`internal`: a read has no key, and the seat makes it again as its schedule
says, and sees a long poll cut short again and again (a proxy that gives a
request less than its wait) for what it is.

A draft of an answer (`conversation_draft`, §5.3) is marked best effort
(`core.WithBestEffort`): the limiter lets it through without a token, and
`Retrying` never sends it again, nor tells the limiter of its 429, which is
the draft's own limit and no reason to slow polling. The drafter decides
alone whether one is worth another try.

### 2.2 The rate limit

One bucket per agent, at 90 % of Core's allowance (540 a minute, burst 90 at
the defaults). Waiting calls are served by priority: answers, then polling,
then events and seats (`core.PriorityOf`). Polling also has a share of its
own (`max_rate_share`, 30 %): each seat's inbox interval is at least
`courses × 60 / (share × rate − event and seat calls a minute)` seconds
(§7.3). A long poll is one call to the bucket and to the share, however
long it waits, and the next begins no sooner after it began than that
floor: idle, a seat that long-polls spends one call per `wait_s`, 2.4 a
minute at 25 s, where the schedule spends 2 to 6 idle and 30 hot.

Drafts are kept off the bucket rather than given one of their own. Core
does not count a draft it carried out against the actor's limit (it gives
the token back), only against its own of 10 a second per conversation (a
draft it refuses counts as any call does, and an actor over its limit is
refused drafts too: a 429, which the drafter drops);
the drafter writes one conversation's draft at most every 300 ms, one
write at a time, so the per-conversation limit is the one that matters,
and a per-agent bucket would have the wrong shape. What Core counts until
it gives the tokens back is at most one draft per answer in progress,
`answer.max_concurrent`, within the 10 % the agent's bucket leaves below
Core's limit. Taking the agent's tokens would only slow its answers and
polls for writes Core does not count.

### 2.3 The catalogue

`GET /v1/tools` (no token) is fetched at start and its hash kept: the sha256
of its canonical JSON. `core.Catalogue` holds each tool's MCP name, kind and
input schema. The permission gates of §4 are kept by hand in `toolset`,
checked at start against the catalogue: a gated tool missing, or no longer
of its gate's kind (a read, or a write), is refused. CI compares the pinned
Core's catalogue with `internal/core/testdata/catalogue.json`. Whether Core's
reads wait for news is read from the catalogue it serves, never from the
snapshot: `Catalogue.MaxWait` is `wait_s`'s maximum in a tool's input
schema, or none on a Core from before 2c1fe1b, whose schemas refuse any
argument they do not name. Whether it takes the drafts of answers is read
the same way: `Catalogue.Drafts` is whether it offers `conversation_draft`,
of kind `ephemeral` (a change that is no action: no key, recorded nowhere,
never proposed); a Core without it is sent no draft, and no model call is
streamed for one. The catalogue is fetched once per Core and worker: after
upgrading Core, restart or reload the runtime for it to long-poll and to
write drafts.

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

**Streaming.** An adapter may also be an `llm.Streamer`: `Stream` makes the
call `Call` would, with `stream` on, tells an `llm.TextFunc` each piece of
the answer's text as it arrives, and returns the same `llm.Response` Call
would have: it builds the stream's pieces up into the body the provider
would have sent whole and reads that as `Call` does, so the parts, the text
byte for byte, the stop, the tool calls and the usage are the same, and so
is everything the loop and the ledger do with them. Reasoning and tool calls
are never told as text. The loop streams only where Core takes drafts (§5.3),
which is what the text is for; otherwise `llm.Stream` calls `Call`.

- `openai_chat` streams Chat Completions' SSE, with
  `stream_options.include_usage`: content, refusal, `reasoning_content`
  (DeepSeek, Kimi, GLM), `reasoning` and `reasoning_details` (OpenRouter,
  joined by type and index), tool calls by index with their arguments
  joined across chunks (or whole, where a server sends each call in one
  chunk), Gemini's `extra_content`, the finish reasons, and the usage from
  the last chunk, or from its choice as Kimi sends it. That is OpenAI,
  Azure, DeepSeek, Qwen, Kimi, GLM, OpenRouter, Gemini's compatible
  endpoint and the local servers.
- `anthropic` streams the Messages API's events: each content block built up
  from its start and its deltas (`text_delta`, `input_json_delta`,
  `thinking_delta`, `signature_delta`; `redacted_thinking` whole), the stop
  reason from `message_delta`, and its usage laid over `message_start`'s.
  Its retry without stale thinking blocks holds for a stream too.
- `gemini` streams `streamGenerateContent?alt=sse`, whose chunks are each a
  response holding the parts that came since the last: the pieces of one
  text, or of one thought, are joined into one part with the signature
  that came on any of them, a `functionCall` comes whole, and the
  `finishReason` and the usage are the last chunk's.
- `bedrock_converse` and `openai_responses` answer whole: a draft of theirs
  shows its steps and no text.

The whole call has the call's timeout, as before. A stream cut off before its
end (`[DONE]`, or at least a finish reason; `message_stop`, or at least a stop
reason; a `finishReason`) is `ErrNetwork`, one out of time `ErrTimeout`, and an error chunk or
event part way (an upstream failing behind OpenRouter, an `overloaded_error`)
is classified as the provider's refusals are: each is retried, then the
fallback, exactly as a failed request is, and the draft's text starts again
from nothing. A provider that refuses to stream with a 400 that names the
stream (an Azure API version that knows no `stream_options`) is called again
whole, and that adapter streams no more; a server that ignores `stream` and
answers whole is read as `Call` reads it.

## 4. Tools

`toolset.Build(catalogue, seat perms, tools config, access, dialect)` is
§4's formula with the product owner's rule for writes: an agent's
permissions are the ones it was seated with, each `denied`,
`confirm_required` or `autonomous`, and the runtime honours them instead of
adding a rule of its own. A tool is offered when its gate is allowed by the
seat's perms (any level but `denied`), it is in `allow` (by default every
gated read and write), not in `deny`, not in the built-in deny list
(below), and it is a read, or a write in a conversation that may have
writes (`toolset.ReadWrite`). Core decides every write again at the seat's
level: at `confirm_required` it comes back `proposed` and waits for a
person, at `autonomous` it is `executed`, and at `denied` it is refused.
Unknown gates offer nothing.

**Gates.** `GET /v1/tools` does not name them, so they are kept by hand from
Core's own declarations of the tools: `toolset.Gates` for the reads (those
of §2.3; `document_versions` on `document_read_draft`, Core then holding the
caller to the document's kind as `document_get` does; `submission_roster`,
where every student in the caller's scope stands on an assignment, those who
have not started among them, on `submission_read`, with names only for a
seat that holds `member_read`; the roster: `member_list` and `member_get` on
`member_read`, `member_lookup_actor` on `member_manage`; the queues of
proposals, `action_list_proposed`, `action_list_pending_review` and
`action_get`, on `action_decide`; and the course's join links,
`course_join_link_list`, on `member_invite`, never a token; each as Core
gates it), `toolset.WriteGates` for the writes: `assignment_create`,
`_update`, `_publish`, `_unpublish` and `component_create`, `_update`,
`_move` on `assignment_write`; `document_create`, `_add_version`,
`_publish`, `_archive`, and `document_update` (a rename, or a place in the
list) and `document_unarchive`, as `document_archive`, on any of
`document_write`, `submission_write` and `grade_submit` (the document's
kind names the one that governs, as reading does); `grade_submit` and
`grade_post` on the permissions of their names, `grade_regrade` on both, at
the lower of their levels, and so `grade_override_total`,
`grade_clear_override` and `grade_comment_total`, which write what a
student is shown of a total at once, as a regrade does;
`grade_undo_ungraded_as_zero` on `grade_post`, as posting as final is;
`submission_create`, `_update_draft`, `_submit` on `submission_write`;
`submission_set_lateness`, `_record_missing` on `grade_submit`;
`member_add`, `member_update_perms`, `member_update_perms_bulk`,
`member_rescope`, `member_pause`, `member_resume`, `member_remove` and
`member_set_role` (a seat's roster role, which grants nothing) on
`member_manage` (Core's `manageMembers`), and `course_update_details` on
it too; `course_join_link_revoke` on `member_invite`; and `action_decide`
and `action_review` on `action_decide`. `course_update_details` changes a
course's title and description from a seat in it, which Core gates on
`member_manage` as its instructors change them: unlike `course_update`,
`_activate`, `_archive` and `_move`, which Core keeps to the platform's and
the departments' administrators and the built-in list denies, it is the
course's own write, gated as the course's other writes are on the
permission Core checks, and offered, as they are, only in its owner's
conversation. Every tool of the pinned catalogue, read or write, is gated
or on the built-in list. At start, and when the catalogue's hash changes,
`CheckCatalogue` refuses a gated tool that has gone, is of another kind than
its gate's, or is on the built-in list. A course tutor, as Core's
`course_tutor` preset seats one, holds none of `document_read_draft`,
`submission_read` and `action_decide`, and is offered none of the reads they
open.

**Managing members.** An agent manages the course's members only when its
seat holds `member_manage`, which only someone who manages them gives
(an instructor, in the built-in presets), and never more than they hold
themselves: Core holds every grant to the granter's own levels, scope and
life. Since 169cf50 every seat also says the most it may hold of each
permission (`perm_ceilings`, with `perm_ceiling_reasons`: Core's
`domain.Ceiling`), and a delegate may hold `member_manage` as far as its
principal does: an instructor's own agent, hosted or not, may be given it
and manages the course's members for its owner, as the seat that an agent
nobody owns has when an instructor seats it with `member.add`, which an
operator runs with `tools.writes` on. A student's own agent may not: its
principal holds none. The member writes go, as every write does, to the
owner's conversations alone. Reads of the roster are offered wherever the
seat's perms allow them, as any read is: a course tutor's `member_read` is
denied (and a seat that holds it is no longer within a student's, so no
student may address it), and so is a student's own agent's, capped by the
student's.

**Deciding proposals.** A seat that holds `action_decide` reads the queues
of proposals and one in full, and, in its owner's conversation as every
write, is offered `action_decide` and `action_review`. Core holds an
agent's `action_decide` at `confirm_required` at most (its ceiling,
`agent_decides_by_proposal`, since 169cf50), so that the model's decision
or review is itself a proposal that a person confirms: a triage
assistant, never a decider. Its own proposals, and what it did at
`pending_review`, its owner decides and reviews where they could have done
the same themselves without anyone's confirmation (`by_owner`), whatever
their own `action_decide`; the runtime follows those decisions from the
events as any other (§5.4). The runtime holds the model's level there again
(`toolset.Decides`): a decision or review from a seat whose `action_decide`
is anything else (`pending_review` would let it take effect before anyone
looked, `autonomous` without anyone looking) is refused before Core, a
`forbidden` result that takes no number, counted as `refused` in
`tool_writes_total` and logged. The prompt (§6) says the model recommends,
when its owner asks, having read the proposal, with a one-line reason.
`action_withdraw` stays denied.

**The seats a model never changes** (`toolset.SeatGuard`). Every `member_*`
write, `member_set_role` among them, is checked before it reaches Core
against the seats of the answer: the agent's own, its principal's (its
owner's own seat in the course) and the conversation's opener's, which in a
conversation offered writes is the principal's for a delegate, and whoever
opened it for an agent nobody owns; and the seats of those people's other
agents. A write that names one of the first three (`member_id`, or any of
`member_ids`, in any case) is refused: its own seat Core refuses too, and
the seat of the person it acts for no text may have it change, whoever
claims to ask. Any other seat it names the runtime reads first with
`member_get`, as the agent, and refuses the write when the seat is a
delegate of one of those people (their other agents, which only their
owner brings in and answers for), or when it cannot read it; a seat Core
says is not in the course is left for Core to say so. Core refuses the same
of a delegate since 169cf50 (`not_your_principal`); the runtime holds every
agent to it, one nobody owns included, before Core. So an agent that
manages members needs `member_read` too, or every write that names a seat
is refused. A change to every seat of a roster role
(`member_update_perms_bulk`, which Core applies to every live seat of the
role but the caller's own) is made only when none of those people's seats
has the role, and none of their other agents' seats: the runtime reads each
one's role with `member_get`, and the role's seats with `member_list`
(200 a page, ten pages at most), as the agent, and refuses the change,
telling the model to change the seats it means one at a time, when a seat
has the role or the roster cannot be read. A refusal is an `is_error` result
(`forbidden`) that reaches nobody, takes no number and spends nothing of
the answer's writes, and is counted as `refused` in `tool_writes_total`
and logged. A runner without the answer's seats refuses every member
write.

**Who is offered writes** (`worker.accessFor`): only a conversation the
agent's owner opened, and only while `tools.writes` is on. In a seat that is
someone's delegate, that is a conversation whose opener is the seat's
principal; anyone else the agent answers (a course tutor's students, or
whoever else may address a delegate that answers the course) is answered
with reads alone, whatever the seat allows, so that no student can talk an
agent into changing grades or material. `tools.writes` is on by default for
a hosted agent, whose owner Core names (the registry writes it out, and its
owner turns it off through `PATCH /agents/{id}`), and off for a YAML agent,
whose owner is whoever its operator says (§5.1): an operator turns it on. A
seat that is nobody's delegate belongs to an agent nobody owns, which has
writes only when its configuration turns them on, and then in whatever
conversation Core lets be opened with it, since Core holds every opener of
one to at least the agent's own permissions; a hosted agent's seat that is
nobody's delegate has no owner to answer with writes.

**The built-in deny list** (`toolset.BuiltinDeny`, §6.1) is never offered,
whatever the configuration and the seat's perms say, and is checked again
by `Run`; each entry has its reason beside it in the code:

- `conversation_*`: the runtime reads and answers conversations itself,
  from the one it is answering; a conversation tool would let the model read
  other people's (a tutor's token reads every one addressed to it), or open,
  answer, close or retract one in someone else's name. `conversation_draft`
  among them: the runtime writes the answer's draft itself (§5.3), and a
  model writing one would show the asker whatever it liked as the answer to
  come.
- `event_list` and `action_list_mine`: the runtime reads them itself, and
  `action_list_mine` returns the agent's own actions, the answers it wrote in
  other people's conversations among them.
- `action_withdraw`: withdrawing acts on any proposal of the agent's, the
  answers the runtime follows among them. (Deciding and reviewing are gated,
  above: the model's decision is a proposal a person confirms, and the
  runtime refuses one that would not be.)
- `actor_*`, `agent_*`, `credential_*`, `me_*`: platform administration of
  actors, the owner's management of their agents, tokens and seats, the
  caller's own tokens and password, and who the caller is and where it sits,
  which the runtime reads itself. A model issuing a token would put a
  credential in text; revoking, suspending or withdrawing would stop the
  agent.
- `member_add_delegate`, `member_delegate_defaults`: seating agents, on
  `agent_delegate`, and previewing what one would be given. An agent never
  seats agents: Core refuses a delegate's, and no model is to multiply
  itself. The rest of `member_*` is gated on the seat's perms (above), and
  kept off the seats a model never changes.
- `member_reset_password`: a temporary password for a member who has
  forgotten theirs, shown once, a credential in the model's text; Core
  refuses an agent's call too (`people_only`).
- `course_create`, `course_update`, `course_activate`, `course_archive`,
  `course_move`, `course_seat_instructor`, `course_list`, `preset_*`,
  `term_*`, `department_*`: the platform's administration, which Core gates
  on a platform or department administrator's role that no course
  permission grants. (`actor_*` covers `actor_invite_new` and
  `actor_lookup_by_email`, the departments' administrators' own.)
- `document_upload_url`: a signed URL for bytes the model cannot send, and a
  credential for the upload besides.
- `document_purge`: removing a document's or a version's text and file for
  good, an administrator's tool, which no course permission grants.
- `course_join_link_create`: a link that seats whoever opens it as a
  student, at once and by no proposal, whose token, returned once, is a
  credential that would be in the model's text. Listing and revoking the
  links are gated on `member_invite`.
- `memory_*`: Core's memory of the agent (about its owner, each asker, the
  course), reached by the conversation a call names. The runtime keeps each
  conversation's memory itself and answers one conversation from that
  conversation alone (§6): the tools would let the model read what is kept
  about other askers by naming their conversations, and write about
  people.

`deny` entries ending in `*` cover every tool they begin. The model sees
each tool through `toolschema`: bound arguments removed (`course_id`,
`idempotency_key`), the common transform, the adapter's dialect; cached per
catalogue hash and dialect.

Running a call (`toolset.Set.Run`): the name must be offered, and not on the
built-in list; a write needs the answer's account of its writes; the
arguments must parse; `toolschema.Reverse` drops nulls the original schema
does not allow and puts the bound arguments back (`course_id` is always the
conversation's course, whatever the model wrote); the result is validated
against Core's own schema; a member write must leave alone the seats the
answer keeps (above). All of a turn's calls are checked in call order
before any is sent. A write is then bound to its key,
`tool:{conversation}:{message}:{attempt}:{n}` (`core.ToolKey`), n its number
among the writes the answer sent, from 1 in the order the model made them;
a key longer than Core's 200 characters is `tool:` and the sha256 of it.
Whatever key the model wrote goes. The same attempt tried again (its model
or its worker failed before it posted) numbers its writes the same, and Core
replays what it did the first time; a new attempt's keys are new, and its
prompt remembers what the earlier one did (§6). A write the answer has
already sent, the same tool and arguments, goes under its first key and
takes no number, so that a model that makes it again, after a proposal or a
timeout, meets Core's replay; one made twice in one turn is made once. At
most `per_answer.max_writes` writes (10) are sent, counted apart from the
reads (every call counts against `tool_calls`); past it, a write is an
`is_error` result saying the answer's writes are spent, and reaches nobody.
Then Core is called. Any failure before Core is an `is_error` result the
model can correct itself from. Results are Core's envelope as JSON, as it
is: `executed`, `proposed` with its `action_id` (not an `is_error`: it is
Core's normal answer at `confirm_required`), `denied` or `failed`, cut at
32 KB keeping `status` and `error` whole; a document's file text too long
for that is given in parts (Files, below). A write Core did not answer may or
may not have been made: the model is told so, and that the same call again
is never made twice. `document_get`'s `download_url` never reaches the
model: the runtime fetches the file (below).

**Files** (`toolset.giveFile`, rule 6). The runtime fetches a document's
file (at most 10 MB, a presigned URL through the egress proxy) and gives it
to the model by what it is, whatever the model; what became of it goes in
the result as `file`: its name, type and size, `given_as` (`file`, `text`
in `file_text`, or `not_given`), `extracted_from` when the text is the
runtime's reading of the file (`ocr` when its OCR recognized it), and a
`note` saying why it was not given or what the text holds and leaves out.

- Text (`text/*`, JSON, Markdown) is its text, to any model.
- An image is a file part where the adapter takes files, and otherwise
  what the runtime's OCR recognizes of it (OCR, below).
- A PowerPoint, Word or Excel file (Office Open XML: `.pptx`, `.docx`,
  `.xlsx`, and their macro-enabled and template forms) is the runtime's text
  of it (`internal/doctext`), to every model: a deck's slides in the order
  the presentation lists them, each `## Slide N: title`, its paragraphs a
  line each, the body's bulleted by level, tables in Markdown, SmartArt as
  its points, pictures and charts only named (`[image]`, `[chart: title]`)
  and counted, then the slide's speaker notes; a document's body in order,
  headings as `#`, lists as `-` and `1.`, tables in Markdown, text boxes
  once, deleted text and field codes left out, its footnotes and endnotes
  after it (`[^n]`), its headers and footers each once; a workbook's sheets
  by name, each's rows as CSV, shared and inline strings resolved, values
  as stored (a formula's cached result, a date's serial number), cut at
  500 rows and 50 columns with its heading saying so.
- A PDF is a file part where the adapter takes files and its provider takes
  a PDF of its size and pages (`llm.FileLimiter`: OpenAI's 32 MB and 100
  pages, Anthropic's 100 pages within what a request's files may take,
  Gemini's 1,000 pages, Converse's 4.5 MB); past them, or for a model that
  takes no files, it is the runtime's text of it, page by page (`## Page
  N`), where that reads as text. A PDF that needs a password to open is
  given to no model. The text is judged over the whole file
  (`doctext.Result.Unreadable`): no text at all, or text on under a tenth
  of its pages and under 200 letters in all (a scan); more than a fifth of
  its characters mapping to nothing (U+FFFD, private use, controls); one
  character over and over (more than 40 % of them, or runs of five or more
  making half the text: the broken ToUnicode map a course's PDF was found
  to have, every glyph `《`); under a fifth of it letters or digits; or
  letters of two unrelated scripts beyond Latin, Greek, Hangul and those
  written with ideographs at 5 % each (glyph numbers taken for
  characters). A PDF whose text does not read is still a file part where
  the model and its provider take it; otherwise it is what the runtime's
  OCR recognizes of it (below), and where there is none, it is not given,
  and the note says it looks scanned, or that its fonts do not map to
  text, and to ask for a version with selectable text.
- An older binary Office file (`.doc`, `.ppt`, `.xls`) is not given: the
  note asks for `.pptx`, `.docx` or `.xlsx`, or a PDF; one encrypted with a
  password, in the same container, is not given either. A file of no type,
  or of one that says nothing (an octet stream, a zip archive), is known by
  what it holds; anything else is not given, with its type named.

**Reading in parts.** A result is at most 32 KB, and a lecture's deck of
38 slides reads as 57 KB of text: cut there, the model would see the
first half and have no way to the rest. So a file's text (the runtime's of
an Office file or a PDF, or a text file's own) longer than a part is given
in parts, and the model asks for the next. A part is at most the result's
limit less 8 KB left for the envelope and the file's record (24 KB of the
text as a JSON string, at the defaults), counted as JSON writes it, so a
part of Chinese, quotes or newlines fits as one of plain letters does.
`toolset.splitText` cuts the text the same way every time, and only by
the limit: where a slide, page or sheet begins, as `doctext` records it
(`Result.Sections`, the offsets of the headings it wrote itself, so a
slide whose text reads `## Slide 9` is still one slide), else after an
empty line, after a line, or at worst between two characters, taking the
first of these that fills at least half a part and the last that fits
otherwise. The parts together are the text exactly: nothing is lost at
their edges and nothing given twice. The file's record says which part
`file_text` is (`part`, `parts`), which slides, pages or sheets it holds
(`part_holds`: `slides 1–16`, `the end of slide 17 to slide 20`), and,
but for the last, the call that reads the next (`next_part`: `{"tool":
"document_get", "arguments": {"document_id", "version_id", "file_part":
2}}`), which its `note` says in words; the first part says how many there
are, and, when there are at most twenty, what each of the others holds,
so that a model looking for one slide asks for its part at once.

Parts are numbered, not asked for by slide or by byte offset: a slide may
be longer than a part, and a range of slides the model chose could be
again too long for one result, while a part always fits one and the first
says how many there are. The model asks with `file_part`, an argument the
runtime adds to `document_get`'s schema as the model is shown it (Core's
own schema naming one of that name fails `Check`, since the two would be
one) and takes out again, with the others checked, before the call goes
to Core: every part is a call of `document_get` with the caller's own
token, so a part is given only of a version the caller may still read,
and a document archived or purged since gives nothing. `next_part` names
the version the first part was of, which Core gives any caller who read
it (a student the published version, by its id), so the parts of one
reading are of one version even when a new one is added meanwhile. A part
past the last, or asked of a text given whole, is not given, and the note
says which there are; `file_part` of a file given as a file part (a PDF
to a model that takes it, an image) is noted and does nothing. Only when
Core's own result leaves a part too little room (a document whose own
text, `body_md`, is kilobytes long besides its file) is the part cut, and
the note says so.

What was read of a file is kept per worker (`toolset.TextCache`), so that
the file is fetched and read once, not once a part: keyed by the version
Core named, its checksum, and the limits it was read within, and kept
only for a model given the text (a file part needs its bytes, which are
never kept). A version's file never changes, and the cache is reached
only after Core has given the caller that version, so every agent of the
worker shares it. It holds at most 32 MiB of text in all, the reading
used least recently going first past that; a reading that ran out of time
is not kept, and neither is one larger than the whole bound.

**OCR** (`internal/ocr`, `toolset.giveOCR`). Much of a school's material
in China is scanned, and many of the models it uses take no files. So a
PDF whose text does not read (`no_text` or `unmapped`, above) and an image
are given to a model that cannot take the file, or whose provider does not
take a PDF of its size or pages, as the text OCR recognizes of it. A model
that takes the file still gets the file part, as before: its own reading
of the pages beats tesseract's, and the runtime spends nothing on it.

- *The engine* (`ocr.Engine`) runs two programs: `pdftoppm` (poppler)
  renders one page at a time (`-r 300 -gray -png`, cropped at 5,000 pixels
  a side), and `tesseract` recognizes it (`-l chi_sim+chi_tra+eng --psm
  3`, the fast models Debian packages). A PDF's first 40 pages are read,
  in order, each under `## Page N` as `doctext` writes pages, so that the
  text is cut into parts where its pages begin; a page with no text is
  marked `[no text found on this page]`, one that could not be read or
  took too long is marked so and the rest go on; notes say which pages
  were left out and why. An image is read whole, and one past 40 million
  pixels (its size read from its header, never decoded, before any program
  runs) is refused. Text is kept valid UTF-8 without controls, at most
  64 KB a page and 2 MB a file.
- *Every file is taken to be hostile.* Each program runs as a subprocess
  of the worker, never in it, and is held from its first instruction by
  `prlimit` (util-linux), which sets the limits on itself and execs it:
  its address space (1 GiB: tesseract with three languages peaks near
  200 MB on a dense page), CPU seconds (the page's timeout), the size of
  any file it writes (256 MB), 256 open files, and no core dump. A
  `setrlimit` wrapper of the runtime's own would do the same, but Go
  cannot set limits between fork and exec for a child alone, and a
  helper binary to do it would be one more thing in the image; prlimit is
  Essential in Debian, so it is there, and where it is not, OCR is off,
  never run without a memory limit. Each program has a hard timeout (90 s
  a page, 15 min a file) whose end kills its whole process group, as the
  worker's stopping does; one thread (`OMP_THREAD_LIMIT=1`) at niceness
  10, so that answering is never starved; an environment of `PATH`,
  `LC_ALL=C`, and `HOME` and `TMPDIR` in a private directory made for the
  file (0700) and removed whatever happens, nothing else of the runtime's
  (no credential, no proxy); its standard output bounded, its standard
  error only counted. It has no network in the sense that matters: it is
  given only files the runtime wrote, by arguments the runtime fixed,
  never a URL (tesseract would fetch one), and neither program follows a
  reference out of a file. A network namespace would need privileges the
  container does not have (Docker's default seccomp profile refuses to
  unshare one), and the deploy runs with `--network host`.
- *In the background, once.* `ocr.Service` is the worker's: at most
  `OCR_CONCURRENCY` files at once (1, at most 8), the others waiting their
  turn, at most `OCR_QUEUE` of them (8: a file waiting holds its bytes),
  past which a file is not started and the model is told to ask later. A
  file is known by its checksum, sha256 of its bytes as the runtime
  fetched them (never Core's, which may name nothing). The first question
  about it takes its lease in the store (`ocr:<sum>`), so that two workers
  do not recognize it at once, starts it, and waits for it `OCR_WAIT`
  (5 s) at most, and never past half the time the answer has left: a
  small file is answered at once; a long one is not given, its record
  says `ocr: "in_progress"` with the pages done so far, and `ask_again` is
  the `document_get` call to make again, naming the version, which the
  note asks for in a minute or so. A question while it runs waits as long
  again, so that a model asking at once is not answered at once, again and
  again; one on a file another worker holds is told the same at once.
- *Kept.* The text (or why there is none: too large, too long, could not
  be rendered or read) is kept in the store's `ocr_text` by the checksum,
  for every worker, and every later question reads it there, through the
  same paging as any other text: 180 days for a text, a day for a failure,
  which is then tried again. What was read is kept in the worker's
  `TextCache` too, under the checksum, so that its parts are read from the
  store once. The cache and the store are reached only after Core has
  given the caller the document, with its own token; the checksum is the
  runtime's own, of the bytes it fetched then, and a file fetched again
  for OCR must have it.
- *What the model sees.* `given_as: "text"`, `extracted_from: "ocr"`, and a
  note that it is the runtime's OCR of the file's pages, why (the model
  takes no files; the PDF has no text of its own), that it may hold
  recognition errors (characters misread or missed, a simplified character
  in its traditional form, lines out of order) and has no pictures or
  layout, and that where a figure, a name or a date matters it should say
  it was read by OCR. When OCR is off, the note says the runtime has no
  OCR here, and why; when it failed, why, and to ask for a version with
  selectable text.
- *Off.* `OCR=auto` (the default) is on where tesseract with its languages,
  pdftoppm and prlimit are installed, as they are in the image, and off,
  with a warning at the start saying what is missing, where they are not;
  `OCR=on` refuses to start (and `check` fails) without them; `OCR=off` is
  off. The start's log line and `check` say which.
- *Counted*: `ocr_requests_total{result}` (a question's outcome: `done`,
  `failed`, `in_progress`, `started`, `busy`, `off`),
  `ocr_jobs_total{kind,outcome}` (`done`, `empty`, `too_large`, `timeout`,
  `failed`, `cancelled`), `ocr_pages_total{kind,outcome}` (`text`, `empty`,
  `failed`), `ocr_job_seconds{kind}`, `ocr_page_seconds{step}` (`render`,
  `recognize`), and the gauges `ocr_jobs_running` and `ocr_jobs_waiting`;
  and one log line a file, with the start of its checksum, its kind, the
  outcome, pages, characters and time, never its text.

`internal/doctext` reads every file as hostile, from memory,
never touching the filesystem nor following a relationship outside the
package: at most 64 MB decompressed in all and 32 MB from any one entry or
stream (a zip or Flate bomb is refused as it inflates, whatever its
headers say), 10,000 archive entries, 500,000 PDF objects, nesting 256
deep (128 for PDF objects), 8 million XML tokens or PDF operators, 2 MB of
text, 2,000 slides, pages or sheets; an entry with an absolute or `..`
name, a name given twice, an encrypted entry, a character set but UTF-8,
and a document type's entities make the file malformed; a limit met, or the
20 s the runtime gives the reading (within the answer's own time, checked
as it goes), ends it, giving what was read with a note when there is some.
PDF is read by a reader of the runtime's own, on the standard library and
`golang.org/x/text`'s character sets: the maintained pure-Go libraries were
weighed and none would do. `github.com/ledongthuc/pdf` and
`github.com/digitorus/pdf` (forks of `rsc.io/pdf`, BSD) loop for ever, and
cannot be stopped, on a 400-byte file whose page tree names itself or
whose `/Parent` chain loops, read no text drawn in a form, and bound
nothing of what they decompress or recurse into; `github.com/pdfcpu/pdfcpu`
(Apache 2.0) has no text extraction, and writes a configuration directory
when used. The runtime's reader walks the page tree once per node,
bounds every reference chain, follows forms (twelve deep, never into one
already drawn), and reads the standard security handler's files anyone may
open (RC4, AES-128 and AES-256, revisions 2 to 6); a file encrypted for a
password or for certificates is refused. It maps codes to text by the
font's ToUnicode CMap, Adobe's predefined Unicode CMaps and those of GBK,
GB 18030, Big Five, Shift-JIS, EUC-JP and EUC-KR, the standard encodings
with their Differences' glyph names, Symbol and the dingbat fonts;
Identity-H with no ToUnicode map maps to nothing, and the judgement above
catches it. `go test -fuzz` runs `FuzzPPTX`, `FuzzDOCX`, `FuzzXLSX`,
`FuzzOfficePart` (one XML part of a sound package), `FuzzPDF`,
`FuzzPDFContent` (a page's content stream) and `FuzzCMap`, seeded with the
tests' files and the hostile ones of `hostile_test.go`.

Every write sent is recorded, in ids, counts and codes, never its
arguments: in the answer's ledger row (the writes sent, and how many Core
executed, proposed, denied and failed), in `tool_writes_total{tool,
outcome}` (those four, `error`, `unreachable`, and `refused` for one past
the budget or one kept off a seat), and in one log line each, with its tool, number, key, status,
error code and action, which with Core's own action log is the audit of
what the agent did. An executed or proposed write is noted in the
conversation's memory. A proposal of a model's write is its owner's to
follow in Core: the events poller settles only the runtime's own answers
and closes, and such a proposal does not hold the actions cursor back.

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
worker that was still running it. A hosted agent's state names the
version of its row the worker had put in force, and the store never
writes a state of an older version over one of a newer: a worker that
read the registry late (every worker writes the state of a paused or a
rejected agent) cannot undo what the agent's holder wrote. Within one
worker, one Core actor is one agent: a second agent configured with the
same token goes to state `error`, naming the first. `SIGHUP` reloads the configuration and the price table:
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
agent was connected by someone who held its token without owning it, or,
in a Core from before 169cf50, where an administrator could re-own an
agent with `actor.set_owner`, it was given to someone else) or none (its
owner was taken away, as such a Core could), it is
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
catalogue, and `me_memberships`.

Core takes conversations in the site for an agent only after the brain
running it declares so, with the agent's own token, and only while that
token is live: `me.site_chat {on: true}`. Until then nobody may open a
conversation with the agent or ask in one (`agent_answers_elsewhere`), so a
question can reach an agent only once a runtime has run it. The runtime sends it once each
time it starts running an agent, right after the first successful
`me_get` for one with an owner (a hosted agent always has one; a YAML
agent when `me_get` names one), else once a seat of its answers
(`conversation_answer` not denied), under a key of its own each time
(`site-chat:` and a random id: after a token's revocation a replay would do
nothing). A call Core did not answer is sent again at the next read of the
seats, within 10 s each time; a refusal is logged and left until the next
start. It is never sent `on: false`: a restart must not flap it, and the
token's revocation (a new token put in, the agent deleted) turns it off in
Core. Core's instructions to an agent say to declare `on: false` when
what runs it stops; the runtime does not, for the reason above: with
workers taking agents over from each other, a stopping worker's `false`
could land after the next worker's `true`. A Core whose catalogue does not
offer the tool (one from before it, as 571e1f9, the runtime's pin before
61b7494, was), or that refuses it as a tool it does not know, has nothing to
declare, which is logged once per catalogue. The model is never offered it
(`me_*` is on the built-in deny list, §4). The fake Core offers it, and
refuses a question to an agent that has not declared, as Core does;
`Options.WithoutSiteChat` answers as a Core from before it.

It reads memberships again every
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
sends where REST answers 401 when the token's actor no longer exists. An
instance whose configuration was replaced while it wound down (a hosted
agent's new token put in force before the old instance's answer in
progress ended) records nothing as it ends: its 401 is most often the old
token's, which the new one revoked, and the agent starts on its new token
at the next lease tick. A reload starts an unauthorized or failed agent
again even when its configuration has not changed, because a new token
goes into the same secret file (`docs/deploying.md`). Tokens and model keys are read when an
agent starts, so a rotated one takes effect at the next start. A paused
agent makes no calls at all.

### 5.2 Polling

Per seat (§7.2):

- **Inbox, long-polled** (`worker/longpoll.go`, `seat.go` `pollInboxOnce`,
  `nextInbox`): where the catalogue Core serves offers `wait_s` on
  `conversation_inbox` (2c1fe1b and later), each call asks Core to wait
  `long_poll_wait_s` (25 s, whole seconds, at most what Core offers) for a
  question, and Core answers as soon as one is committed: against 2c1fe1b
  the question is claimed some 20 ms after it is written. A call that comes back empty is made again at once, no
  sooner after the last began than the rate share's floor, and while the
  agent is slowed after a 429 no sooner than its doubled schedule
  (`LongPollNext`). Core answers at once while questions wait, whether or
  not they are being answered, so a call that found rows is followed by a
  poll on the schedule below, and a wake-up (an answer posted, the opener
  writing, a proposal settled, a hold lifted) brings the next call forward
  to now. Stopping the worker, or a seat, cancels its call at once.
- **How many wait**: an agent's calls waiting for news, inboxes and events
  together, are at most `long_poll_max` (12; Core lets one actor have 16,
  `LONG_POLL_WAITERS_PER_ACTOR`, and answers any past that at once).
  `Agent.takeLongPoll` gives the places to the seats most recently active
  (a question found, an answer posted, the opener writing), ties by member
  id; the rest poll on the schedule, and a seat that has lost its rank
  gives its place back when its call returns, within `wait_s`. An agent in
  more courses than that long-polls the busiest and polls the others.
- **Falling back** (`Seat.fallBack`): a call that comes back empty in under
  half its wait, not for the seat stopping, is Core not waiting (its bound
  per actor or in all reached, or Core stopping); one that did not come back
  is cut short (a proxy that gives a request less than `wait_s` plus 15 s);
  one Core refused with `invalid_argument` naming `wait_s` (an older Core
  behind the URL than its catalogue said, over MCP an envelope, over REST
  a 400) is made again at once without it, and counted as no error. After
  any of the three the seat polls on the schedule for a minute
  (`Timing.LongPollFallback`, jittered), then tries again;
  `long_poll_fallbacks_total{why=early|cut|refused}` counts them, and
  `/status` shows a seat's `scheduled_until`.
- **Inbox, on the schedule**, against a Core without `wait_s`, with
  `long_poll_wait_s` or `long_poll_max` 0, and for the seats above: every
  `inbox_hot_s` for `hot_window_s` after activity (an answer posted, an
  event of the opener writing), else `inbox_idle_s`, growing ×1.5 per empty
  poll to `inbox_max_s`; every interval jittered by `±jitter`, and never
  below the rate share's floor; the first poll of each seat spread over the
  idle interval. After a 429, the inbox, events and seats intervals all
  double for five minutes. A poll counts as empty only when it finds
  nothing new: rows waiting for a slot do not slow the next. After polls
  that failed, the schedule and a backoff of 1, 2, 4 … 60 s.
- **What is no news** to a waiting call, a seat's perms changed or a token
  revoked, is seen when it reads again at the end of its wait, within
  `wait_s`; me_memberships, every `memberships_s`, may see it first, and
  stop the seat.
- **Events**: every `events_s`, and after a `proposed` answer at once, then
  after 5, 15 and 45 s. Where `event_list` takes `wait_s` and a place is
  left over by the inboxes, the follow-ups' 45 s are long-polled instead
  (`pollEventsOnce`): each read waits for news until the window closes, and
  the next begins at once but no sooner than `inbox_hot_s` after the last,
  so that a busy feed is not read without pause; a decision made in the
  window is settled within milliseconds. The background read every
  `events_s` stays at 45 s: the inbox's long poll sees the decisions that
  put a question back (rejected, cancelled, failed), but only the feed
  tells a proposal approved and posted after the window, and a message
  retracted, whose memory is to be forgotten promptly (§6.3). The cursor
  (`next_seq`) is kept in the store per seat, and moves past a page only
  when every event on it was recorded; a store that fails on one has the
  page read again. The real Core does not move `next_seq` over events the
  actor cannot see, so a page with no events ends the round.
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
   every model call. On an offer of the school's plan (§11.2), the plan's
   quotas stand in for the tenant's, after the agent's own: the asker's
   (agent, course, P) and the owner's (the tenant, across their agents),
   both on the school's key alone; and the plan's ceiling, when set, holds
   every answer on the school's key. Days are UTC. One of the school's
   spent, the model's fallback answers when it is on the owner's own key
   (D8), and its calls and answer are the owner's, not the school's. Out of
   quota: the canned notice under the answer's key, with no model call
   (`on_quota_exhausted: canned`; the plan's own notice for its quotas,
   `runtime.school.on_quota_text` or the built-in one, in the language
   `answer_language` fixes, else in English and Traditional Chinese), or
   skip until tomorrow (`silent`). An answer is on the key of the model
   that wrote it.
5. **Read** X: `conversation_messages` (the newest `history_messages`). If
   the opener's newest message is no longer M, answer that one instead.
6. **Prompt**: the system prompt (§6 below), the seat's facts, X's memory,
   and the history as turns: the opener's messages as `user`, the agent's as
   `assistant`, retracted ones as `[message retracted]`.
7. **Loop** (§7.1) with the seat's toolset, its writes only when the
   owner opened X (§4), bounded by `per_answer`; the writes, by
   `max_writes`, each keyed for this attempt. Stop
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
| `proposed` | keep the action id (state `proposed`); events polled now, +5, +15, +45 s, or long-polled for those 45 s (§5.2) |
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

11. **Ledger**: a row per model call and one per answer, with the writes
    the model made counted by what Core said of them; metrics; release the
    lease.

Every claimed message ends answered, proposed, closed, or with a recorded
outcome.

**Drafts.** Where the catalogue offers `conversation_draft` (§2.3), the asker
watches the answer come, as in Claude Code: while the loop of step 7 works,
the runtime keeps the attempt's draft (`worker/draft.go`) and writes it to
Core, which shows it to whoever reads the conversation until the answer
takes its place.

- The draft of an attempt has an id of its own (`attempt`, a fresh UUID
  for each attempt, so for each pass of step 7) and a `version` that rises
  with each write of it. Its steps: each model call starts a `thinking`
  step, the steps before it done; the first piece of text the call streams
  ends it and starts `writing`; each tool call is a step of its kind,
  `running` until Core answers it (`toolset.Runner.Seen`) and then `done`:
  `document_get` `reading_document`, `document_list` `listing_documents`,
  `assignment_get` `reading_assignment`, `submission_get`
  `reading_submission`, `memory_search` `searching_memory`, any other
  `tool`. A step's `target` is the title of what it read only where every
  member of the course may read it: published course material, a published
  assignment; a rubric's, a submission's, an unpublished document's title
  would tell the asker of what they may not see. The latest 20 steps are
  kept. Its text is the current model call's text so far (streamed, §3),
  replaced whole with each write, at most 20,000 characters; the next call
  starts it from nothing, and so does a try made again after a stream cut
  off or a provider's failure, or by the fallback. An adapter that does not
  stream shows steps alone.
- It is written through one drafter per conversation (so one write in
  flight per conversation, across the attempts and claims of it): the loop
  only changes the draft's state under a lock and wakes it, and its
  goroutine sends the state as it stands at most every 300 ms
  (`Timing.DraftEvery`), the first at once; a state that changes again
  before it is sent is sent once, the latest winning. Nothing the loop does
  waits on it.
- Best effort: a write Core refuses as too soon (its 429, `draft_rate`) is
  dropped, not sent again; one refused because the conversation no longer
  waits for an answer (the answer just went in) is dropped too, and the
  attempt writes no more; one that failed on the way (a 5xx, a timeout of
  its own 5 s) is sent once more, with the state as it stands then, and
  then given up; any other refusal stops the attempt's drafts. None of it
  fails, holds back or slows the answer, or slows the agent's polling.
- Its end: when the answer is to be posted (step 9), nothing more of the
  draft is sent, and Core clears it as it posts or proposes the answer. An
  attempt that ends otherwise (the providers failed, the claim's time ran
  out, the post did not go in, the opener moved on) is ended with `done`,
  which deletes the draft, when any of it was sent.
- A draft carries the model's own text, before step 8's safety pass: Core
  shows it to the asker only where the answer would post without anyone's
  confirmation (`conversation_answer` autonomous), and otherwise to those
  who could approve it (`text_hidden` for the asker, who sees the steps);
  the posted answer, made safe, replaces it. Nothing else goes into a
  draft: never a tool result's content, the system prompt, a key, or
  anything of the memory; steps are a kind and, at most, a title.
- `draft_writes_total{agent, outcome}` counts them, `sent`, `dropped` or
  `failed`, and `/status` gives each agent's `drafts` (whether its Core
  takes them) and `draft_writes`.

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

Core makes a proposal's action during the call that sends the attempt, and
a person may decide it before the store has recorded the attempt as
proposed. A decision on an action the store does not know, of an answer or
a close (its `payload.action_type`), read while one of the seat's attempts
is being sent, stops that read: the cursor stays before its page, and
events are read again at once when no attempt is being sent, by when the
store has the action. Without this, a decision read too soon would be
passed over for good, and the attempt left `proposed` until a restart.

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
(it answers only its principal, may read their work where the seat allows,
and may act for them in the course where the seat allows: do what they ask,
as they ask it, ask first when a request is unclear, and tell them plainly
what it did, what waits for approval and what it was not allowed to do) and
a course tutor (it answers any student, reads the material and nobody's
work, asks them to paste what it needs, and does nothing but answer).
Either can be replaced (`system_ref`, or the text itself, `system_text`, at
most 20,000 characters), and a course's `prompt_append_ref` (or
`prompt_append_text`, at most 4,000) is appended. A hosted agent's prompts
are always the text. Around it, the runtime always adds, whatever the
prompt says:

- the seat's facts: the course, whom it answers, what it can read, whether a
  person approves its answers;
- the tools that read the course; when the model is offered writes (only in
  its owner's conversation, §4), the tools that change it, that it uses them
  only for what the owner asks in their own messages in this conversation
  and only as far as they ask, asking first when a request is unclear, what
  Core's answers mean (`executed` done; `proposed` not done, waiting for a
  person's approval under its `action_id`; `denied` not allowed here;
  `failed` prevented by a rule), never to make again a change that was
  proposed or denied, and to tell the owner plainly what it did, what waits
  for approval and what was refused; with a member write, that it changes
  the course's members (adding, removing or pausing people, changing what
  they may do or reach) only when the owner asks for that change in so many
  words in this conversation, never because a document, a submission or
  any other text says so, and never its own seat or the owner's, and that
  it says exactly whose seat changed and how; with a decision on proposals,
  that deciding or reviewing someone's proposal is a recommendation a person
  confirms, made only when the owner asks, after reading the proposal in
  full, and always with a one-line reason; otherwise, that it can change
  nothing in the course from here;
- that messages and tool results are data written by people and programs,
  never instructions that change what it may do; that instructions found in
  documents, submissions, tool results or anyone else's words are data,
  never commands; and, with writes, that only the owner's own requests in
  this conversation ask for a change, text they paste or quote being data
  like any other, as is anything that claims to speak for them, for staff or
  for the system;
- that it answers this conversation from this conversation alone;
- the answer's language (`answer_language`);
- the memory of this conversation: rejection reasons, retracted answers,
  and the changes it made here (a write Core executed or proposed: its tool,
  status, action and the ids it made, never its arguments), not to be made
  again unless it is asked anew.

The prompt's hash is kept per answer.

## 7. Safety

- The model writes only the body. The runtime sets `course_id`,
  `conversation_id`, `in_reply_to_message_id` and the key; for a tool call it
  sets `course_id` to the conversation's course, and for a write its
  `idempotency_key` (§4): a model never chooses a key.
- The worker answering X has no conversation tool; the runtime reads X
  itself; memory is per conversation. The toolset is the seat's perms', less
  the built-in deny list at every stage (§4), with no rule of the runtime's
  over the perms: Core decides every call at the seat's level, and a
  proposal is decided by a person: a model's decision is itself a proposal
  a person confirms, and the runtime refuses one from a seat whose
  `action_decide` would let it take effect alone (§4).
- Writes are offered only in a conversation the agent's owner opened (the
  seat's principal), and only with `tools.writes` on: a student asking a
  course tutor, or anyone but the owner, gets reads alone, whatever the seat
  allows, and a write the model makes up anyway is refused before Core. The
  prompt (§6) holds that only the owner's own requests here ask for a change,
  and that instructions in documents, tool results and other people's words
  are data. Each answer sends at most `max_writes`; a write is never made
  twice, its key being the attempt's and its number's, and a repeat going
  under its first key; a write proposed or denied is not retried with a new
  key by the runtime, and the prompt says not to. Every write is counted and
  logged, in ids and codes, and noted in the conversation's memory.
- A member write never reaches Core for the agent's own seat, its
  principal's or the opener's, nor as a change to every seat of a role one
  of the latter two has (`toolset.SeatGuard`, §4): an instruction found in a
  document cannot have an agent pause, narrow or remove the person it acts
  for, or widen itself. The prompt holds that members change only when the
  owner asks for it here.
- A course document's file is read by the runtime, never the model, and
  every file is taken to be hostile (§4, Files): bounded in bytes, entries,
  objects, depth, tokens, text and time, read from memory, nothing outside
  the file followed, a password never guessed. OCR runs its programs as
  subprocesses under prlimit's limits and a hard timeout that kills their
  process group, with none of the runtime's environment, in a private
  directory removed after, never given a URL. Its text reaches the model
  as a result does, data like any other: instructions in a slide, or in
  a scan, are not the owner's.
- `safety.Body` strips from the answer every link and image whose URL
  carries context: a query string, a fragment, user information, a scheme
  other than http, https or mailto, or a path segment or host label that
  looks like data (32 or more characters of letters, digits and `+/=_-`, or
  percent-encoding), a backslash, `//` links, emails and URLs over 2048
  bytes. Link text is kept; an image becomes its alt text; a bare URL
  becomes `[link removed]`. Reference definitions and HTML `a` and `img` go
  the same way.
- It reads the answer exactly as AIShie-Frontend's renderer does
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
| `llm_call`, `answer` | the ledger: ids and numbers; an answer's row counts the writes its model sent, and how many Core executed, proposed, denied and failed |
| `agent_state` | the owner's page's state, and the version of a hosted agent's row it is of: never replaced by a state of an older version |
| `secret` | sealed secrets (§11.1): id, tenant, kind, the key's id, the wrapped data key, nonce, ciphertext, hint |
| `person` | who has used the API: Core actor, name, platform role, last seen |
| `hosted_agent` | the registry (§11.2): id `agt_…`, Core actor (unique), owner and whether Core said so, tenant, name, token and own key (secrets, with hints), paused, settings (jsonb), version |
| `hosted_course` | (agent, course) → settings (jsonb), who wrote them, when |
| `registry_rev` | one row: the revision every write to `hosted_agent` or `hosted_course` moves on, by trigger, with `NOTIFY aishie_registry` |
| `audit` | the API's audit (§11.4): when, who, with which of Core's sessions, from where, what, to what, the outcome, and a detail of ids, hints, providers, models and results; kept 400 days |
| `ocr_text` | what OCR recognized of a file (§4, OCR), by the sha256 of its bytes: done or failed, pdf or image, the text (at most 4 MB), pages recognized and of how many, where each begins, notes, why it failed, the engine and how long it took; kept 180 days, a failure a day |

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
the read tools of §2.3 and every gated read (a document's versions, where
students stand on an assignment, the roster, the queues of proposals) and
the gated writes allowed, writes off (`tools.writes`, on for a hosted
agent), four in parallel; three attempts,
then close; the canned notice when out of quota; 19,000 characters; the
newest 30 messages; eight answers at once per agent, four per course; per
answer 8 turns, 12 tool calls of which at most 10 writes (`max_writes`),
150,000 input and 4,000 output tokens, 90 s; no daily
quotas unless set (a school key requires per-agent and per-asker ones, but
an offer of the school's plan, whose quotas are 100 answers a day per
owner and 20 per asker unless `runtime.school` sets them);
polling 2 s hot for 120 s, 10 s idle to 30 s, events 45 s, seats 300 s,
±25 %, 30 % of 600 a minute, and where Core offers `wait_s` each inbox
long-polled for 25 s (`long_poll_wait_s`), at most 12 calls of an agent's
waiting at once (`long_poll_max`); where Core takes drafts, each answer's
draft written at most every 300 ms, one write in flight per conversation, a
write given 5 s and one retry; memory on, purged 30 days after a seat goes;
files of at most 10 MB, read within `doctext.DefaultLimits` and 20 s, their
text given in parts of 24 KB within results of 32 KB, what was read kept
per worker up to 32 MiB; OCR on where its programs are, in
`chi_sim+chi_tra+eng`, 40 pages at 300 dpi, 90 s a page and 15 min a file,
1 GiB a program, one file at a time per worker and eight waiting, the first
question waiting 5 s for it.

## 10. Tests

- `fakecore`: the MCP surface and envelope of Core, scriptable: questions,
  follow-ups written during generation, levels changed, seats paused or
  removed, proposals approved, rejected or expired, retractions, 429s and
  401s; reads that wait for news (`wait_s`), woken by the news Core's
  filters let through, within Core's bounds on calls waiting, and a Core
  from before them (`WithoutWait`); the drafts of answers
  (`conversation_draft`, an ephemeral write) and the views that show them,
  and a Core from before them (`WithoutDraft`); a question its opener
  withdraws, retracting their latest message, which waits for no answer:
  the views say `answered`, its draft goes, and a draft or an answer to it
  is refused (`conversation_not_awaiting`, `moved_on` naming no message),
  as AIShie-Core #42 has it, and the pinned Core, whose withdrawn question
  still waits and takes an answer (`WithdrawnWaits`); each seat's
  ceilings, as Core works them out; an agent's owner
  deciding and reviewing what it did where they could do it themselves;
  and no question to an agent that has not declared it answers in the site
  (the worker's tests wait for the runtime's declaration, or, asking
  before a worker starts, make it as an earlier run would have).
  `internal/fakecore/testdata/fixtures` are envelopes recorded from the
  pinned Core for every row of §2.4 and more (`make record-fixtures`
  against a live Core, whose recorder declares each agent's site chat with
  its token); a conformance test holds the fake to them, and Core's own
  client is tested live against the real one. Four, `drafts`,
  `staff_retract`, `retracted_pending` and `withdrawn`, are recorded from
  #42's Core, ahead of the pin, until the pin moves to it.
- Adapters: golden translations both ways in `testdata/`, every stop reason
  and usage field; `LIVE=1` runs them against the real providers whose keys
  are set, with one request declaring every tool at 16 output tokens, and
  one answer streamed. `openai_chat`, `anthropic` and `gemini` have goldens
  of streams written in their providers' SSE format (`testdata/stream`): text
  in pieces with keep-alives, reasoning streamed before the answer,
  parallel calls whose arguments are split and interleaved across chunks,
  the usage alone in the last chunk and in its choice, a refusal with CRLF
  endings, a stream ending at its finish reason without `[DONE]`, an error
  part way and one cut off; for each answer also written whole, `Stream`
  must make exactly what `Call` makes of it.
- Drafts: the drafter coalesces (fewer writes than changes, each the whole
  state, versions rising, one in flight, spaced), keeps its steps, ends an
  attempt with `done` unless its answer went in, drops a 429 and sends
  nothing twice for it, sends a write that failed on the way once more, and
  stops an attempt Core refuses. The fake Core carries `conversation_draft`
  out with Core's rules (the respondent alone, while the conversation
  waits and its opener may address it, newer writes only, done, cleared by
  the answer posted or proposed and by closing, 10 a second, no rate
  charged), shows the draft in the conversation's views with its text to
  whom Core shows it, and wakes a reader waiting with `seen_draft_version`;
  the `drafts` fixture, recorded from Core, holds it to that, and
  `WithoutDraft` is a Core from before. Against it, a streamed answer's
  drafts show thinking, the syllabus read by its title and the text
  growing, and the answer takes their place; a Core without the tool gets
  none, and no call is streamed; a stream cut off starts the text again;
  drafts that take seconds, or are refused as too soon, or fail, never
  hold the answer back. The end to end (`drafts-shown`, skipped against a
  Core without the tool) watches them as the site does, by long polls with
  `seen_draft_version`.
- `toolschema`: every tool of the pinned catalogue through every dialect and
  back through Core's schema.
- `doctext`: decks, documents, workbooks and PDFs made byte by byte
  (`doctexttest`) read as the model gets them: slides in the presentation's
  order whatever their parts are named, their placeholders, tables, charts
  and notes; headings, lists, tables, text boxes, footnotes, headers and
  footers once; sheets as CSV, cut and said so; where each slide, page and
  sheet begins, and a slide whose text reads like a heading still one;
  PDFs uncompressed,
  compressed, in object streams, encrypted with RC4 and AES-256, with pages
  nested, and with their cross-reference damaged and rebuilt; CJK through
  ToUnicode and the predefined national CMaps; a broken ToUnicode map (`《`
  for every glyph), none at all, and a scan judged unreadable, a scan's
  recognized text and some scanned pages not. Hostile files: page trees
  and `/Parent` chains that loop, a stream its own length, two thousand
  lengths each the next, a hundred thousand nested arrays, a Flate bomb, an
  xref its own `/Prev`, forms drawing themselves and each other twenty-four
  deep, zip bombs, entries past the limits, lied sizes, absolute, `..` and
  doubled names, encrypted entries, older and encrypted Office files,
  entities and other character sets; a deadline past stops the reading at
  once. Seven fuzz targets hold every reader to no panic, its time, its
  text's size and UTF-8, and errors only of its kinds.
- `toolset`: `Build` offers the writes the seat's perms allow only with
  `ReadWrite` and `tools.writes`, none at `denied`, and no tool of the
  built-in list whatever the perms and `allow` say, with every write of the
  catalogue gated or denied; `Run` binds a write to its key whatever the
  model wrote, numbers writes in call order across turns, keys the same
  attempt the same and the next anew, sends a repeat under its first key,
  refuses writes past `max_writes`, and gives `proposed` as it is, not as an
  error; the member writes and the roster are offered on their gates alone,
  `member_add_delegate` and `member_delegate_defaults` never, and
  `SeatGuard` refuses a member write on the agent's own seat, its
  principal's or the opener's, or on another agent of theirs (read with
  `member_get`, `member_set_role` of a sibling's role among them), or on a
  seat it cannot read, and a role-wide change when one of those people's
  seats, or one of their other agents' (read with `member_list`), has the
  role or the roster cannot be read, before Core and without spending the
  budget; a decision or review is sent only from a
  seat whose `action_decide` is `confirm_required`, and refused, counted
  and unnumbered, at `pending_review` and `autonomous`; every tool of the
  catalogue, read or write, is gated or denied, and a course tutor is
  offered none of the roster of submissions, drafts or proposals. `Run`
  gives a file by what it is: text, an image to a model that takes files,
  an Office file as its text, a PDF as a file within its provider's limits
  and as its text past them or to a model that takes none, a scan or a PDF
  whose fonts do not map as a file where it may be one and otherwise as
  what OCR recognized (an image likewise), marked `ocr`, or, where OCR
  gives nothing, not at all with its reason (in progress with the call to
  ask again, not started, failed, no text found, no OCR here, and an image
  then not even fetched), one that needs a password to no model, an older
  Office file unfetched. A scan's OCR, 51 KB of it, is read in parts cut
  where its pages begin, OCR asked once and the file fetched once; a
  question while it is recognized is told so with `ask_again`, and asked
  again it is given; a file fetched again for OCR must be the one read. A long deck is
  read in parts, each asked for with the call the one before names, until
  the last: the parts are the deck's text exactly, each beginning where a
  slide does, the first saying how many there are, Core asked for each
  (for the version the first named, never with `file_part`), and the file
  fetched once; a slide longer than a part is cut on its lines, each part
  saying which end of it it holds; a part past the last, a part of a text
  given whole, and one of a file given as a file are said so, and a
  `file_part` that is no whole number from 1 reaches nobody. `splitText`
  holds random texts of every escape to its promises. The fake Core
  carries out `document_create` through its pipeline, held to the
  `model_writes` fixture recorded from Core; `member_add`, `member_get`,
  `member_list` and `member_lookup_actor`, held to `member_writes`; and
  `submission_roster` and `document_versions`, held to `roster_reads`; and
  serves the files `AddFile` puts in a course.
- `ocr`: the engine against programs of the test's own (shell scripts
  standing in for pdftoppm and tesseract) under the real prlimit: the
  pages read, their headings, the pages past `MaxPages` left out and said
  so, a page that fails or passes its time marked and the rest read, a
  program that sleeps killed with its whole process group at its timeout,
  one that allocates past its memory limit refused it, the environment
  each sees (nothing of the runtime's), its niceness and working
  directory, an image refused by its header's size before any program
  runs, and the private directory removed. The service: a file recognized
  once, in the background, and fetched once, the first question told its
  progress, a question meanwhile waiting as the first did, then the text
  read from the store; the wait never past half an answer's time left;
  turns and a full queue; a file another worker holds; failures kept and
  tried again; nothing kept when the process stops. With the real
  programs (`TestRecognizeScannedCJK`, skipped only where they are not
  installed, and never with `OCR_REQUIRED=1`): a scanned notice drawn in
  Unifont's glyphs, in traditional and simplified Chinese and in English,
  in a PDF that `doctext` judges `no_text`, recognized page by page with
  its blank page said so, and as an image; and their limits.
  `scripts/image_test.sh` (`make docker-test`, in CI) runs the built image
  as the deploy does (65532:65532, no network), `check` with `OCR=on` in
  it, and the same real tests against the programs in the image.
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
  budgets, proposals followed, retractions; long polls (§5.2): a question
  claimed at once by an inbox waiting 20 s, an older Core polled on the
  schedule, one behind a newer catalogue refusing `wait_s` over MCP and
  REST, Core not waiting and a long poll cut short sending the seat to its
  schedule and back, four seats with `long_poll_max` 2 never waiting more
  than two at once and the seat that just answered among them, a seat
  stopping and a worker stopping ending their waits at once, a 429's
  slowdown spacing long polls, a long poll outlasting the client's own
  timeout, and a decision on a proposal settled at once; and the load test
  on the schedule and long-polling; no token in any log line; the
  safety evaluations (injected instructions to call other tools, to answer
  about other students, to post links carrying data); an owner's write
  proposed then executed, counted, logged without its arguments and
  remembered; a course tutor's student offered no write, one made up
  refused; an attempt tried again replaying its write, the next keying it
  anew; the write budget; who is offered writes (`accessFor`); an agent
  that manages members seating one person on a proposal and the next at
  once, and refused, before Core, the changes a document orders to the
  opener's seat, its own and every instructor's; the roster offered as the
  seat's perms allow, and no member write to a delegate; a deck, a reading
  and a scan read by a model that takes files, a PDF of one page at most
  (its adapter's `FileLimits`), and by one that takes none, each answered
  from the runtime's text; a deck of 38 slides read in parts by a model
  that follows `next_part` to the last and answers, the parts its text
  exactly and the file fetched once; a scanned handout read by a model
  that takes no files: told its OCR is in progress, it asks again with
  `ask_again` and answers from the text, the file fetched and recognized
  once and the text kept by its checksum; the site chat declared once per start, by an
  agent with an owner at once and by one nobody owns once a seat of its
  answers, never taken back, and not sent to a Core that does not offer
  it; and Sato's own assistant, given `member_manage`, offered
  `member_set_role` in his conversation, seating Aoi when he asks, and
  refused before Core the role a document orders for his course tutor,
  his other agent, and for his own seat, and a pause of its own.
- `e2e`: the pinned Core (`scripts/ci-core.sh`), agents seated over REST, the
  runtime with the scripted OpenAI Chat server behind the real `openai_chat`
  adapter: a student's own agent answers within the latency target and a
  course tutor keeps askers apart; an idle tutor long-polling its inbox
  claims each question within a second of its being written (against
  2c1fe1b, some 20 ms), with a schedule that would take 10 s; moved on and duplicates across two
  workers are safe (an agent asked before the runtime starts is one an
  earlier run declared; every other is asked once the runtime has declared
  that it answers in the site, which Core requires); a seat set to
  `denied` stops polling and answers again
  when restored; a proposal approved is recorded, and a rejection's reason
  reaches the next attempt; no token or key in any log, before or after
  redaction; the binary's `catalogue --check` and `check --live`; and
  writes: Sato asks his own agent, which holds `document_write`, to create a
  document, which at `confirm_required` is proposed, as the answer says, and
  at `autonomous` is executed and in Core, while a student asking his
  course tutor, which holds a write, is offered none and nothing is
  written; and members: Core seats Sato's own agent with `member_manage`
  and refuses it to Yuki's (`principal_level`); an
  agent nobody owns that Sato seated with it seats Aoi as a student when
  he asks, executed at `autonomous` and in Core, and Ren on a proposal at
  `confirm_required`; told by a document of Sato's to pause his seat, raise
  its own and lower every instructor's, it tries, and the runtime refuses
  all three before Core, and no seat changes; and documents: Sato uploads
  his week's slides as a `.pptx` through Core's upload URL, and Yuki's
  hosted helper, asked about a slide, reads it with `document_get` and
  answers from the runtime's text of it, the download URL never reaching
  its model.

## 11. Hosted agents

M2 lets people connect their own agents from AIShie-Frontend instead of
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
  the school's); on the agent's model on key source `school`, the settings
  of the offer of the school's plan it names (`offer`, and nothing else:
  §11.2's key pool), its key's reference among them, written in the
  document and never in the row; and `tools.writes` true when the settings do not set it,
  since a hosted agent's owner is known (§4), whatever the runtime's
  defaults say. Each course's row is its
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
  is on the owner's key source and has the owner's key and no other, or,
  for the agent's own model alone, is the offer of the school's plan it
  names, with the offer's key, as
  merged and decoded, never one it would inherit from the
  runtime's defaults (a fallback it does not set is none, not the
  defaults'); none is called with the runtime's own credentials (Bedrock
  without a key would sign with the host's) or at a server that takes no
  key; `base_url` is empty (the adapter's own) or an official provider's
  endpoint (§3.9) over https, the provider's own host where it serves
  from a cloud's domain (DashScope's under `aliyuncs.com`, Bedrock's
  runtime under `amazonaws.com`, not a bucket or function anyone can
  name there); and it sends no extra headers (D9). An offer's endpoint
  and settings are the operator's, and its calls go over the runtime's
  own client, as a YAML agent's do.
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
- **The key pool (D8).** The school's AI plan is `runtime.school`: the
  models the school offers (`offers`, each an id, a label people are
  shown, and a model section whose key is a reference under
  `secret://school/keys/`, a file on the runtime's host), checked against
  `allowed_models` and `denied_models` as any model on the school's key;
  and its quotas, in answers a UTC day, `per_owner_day` (100 unless set)
  and `per_asker_day` (20 unless set), and, when set, `per_day`, a ceiling
  on everything on the school's key. Each may also be in dollars, which
  `run` and `check` refuse without a price table that prices every offer;
  in answers alone, an offer needs no price, and its cost is unknown. One
  plan for the school, with no budgets of a department's or a course's
  yet: they would stand beside these quotas. A hosted agent's row on the
  plan is `model: {key_source: school, offer: <id>, fallback: <the
  owner's own model>}`: the registry writes the offer's settings over it
  in the document (§11.2's first point), and refuses an offer the school
  no longer has, settings of the offer's the row sets itself, a course's
  model on the school's key and a fallback on it; `config` holds a section
  that names an offer to calling what the offer does with its key, and to
  a tenant, the owner's, whose quota is the plan's. The worker falls back
  to the owner's model when the offer's provider cannot be reached, and,
  when a quota of the school's is spent (§5.3 step 4), answers on the
  owner's key; without an owner's model, the plan's notice is posted.
- `check`, with `DATABASE_URL`, reads the registry as `run` does, lists
  the hosted agents it would run and those it would not, with why, and
  passes: they keep no other from running. A registry it cannot read (a
  schema older than the binary's, before a deploy's `migrate up`) is said,
  and passes too. `check --live` connects the hosted agents as well.

### 11.3 Where M2 departs from the handout

- **The UI lives in AIShie-Frontend** (the product owner's D1), not in a
  `runtime-web` of the runtime's (§8.3): the runtime serves a versioned
  JSON API only (§11.4), and no HTML. People authenticate to it with a
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
  minutes by default, fifteen at most). `GET /me` says whether they are
  one. Beside an owner's routes, an administrator reads `GET
  /admin/school-plan/usage` alone: today's use of the school's key (the
  store's `Reports.TenantUsage`), the plan's quotas, the total, and a row
  per tenant, a hosted agent's owner's with their actor id and the name
  the runtime last saw; anyone else is 403 `not_admin`.
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
  with `Retry-After`; the assertion; a query (none is taken, but
  `DELETE`'s `revoke_token`: a route that reads a body refuses one too)
  and a body (JSON only, at most 64 KB, no key twice, in one case or in
  two, no member the route does not take by exactly its name, since
  `encoding/json` alone reads a member into a field whose name it matches
  in any case, nothing after the object; a route of no body takes an
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
  and a vault, as `run` always gives it; and the school's plan, when the
  runtime's settings offer a model on it).
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
    and purged, once Core, asked again with the token just before, still
    says the agent is the caller's; one the operator's YAML runs is
    `operator_agent`. Both answers list the agent's other live tokens
    (`other_tokens`, with Core's `credential_list`) and whether one was
    used in the last 15 minutes, for the front end to warn that an agent
    has one brain at a time.
  - `GET /agents` and `GET /agents/{id}`: the agent as its owner reads
    it, with its `version` as a strong ETag, its seats, the proposals
    waiting, today's answers and cost, its model and key hint (on the
    school's plan, `model.school`: the offer's id, label, provider and
    model, whether the school still offers it, and whether the owner's
    model and key stand behind it; and `today.school`: the owner's
    answers on the school's key today across their agents, `used` against
    `limit` with `scope` `owner`, the cost against the quota's dollars
    when it has any, and `per_asker_limit`), whether
    its model may act for its owner (`tools.writes`), and a
    `status` the API works out from the row and the worker's state: paused,
    then no model (`needs_model`), then `starting` until the worker has
    written a state for the row's version, then the state's own, with the
    reason the worker wrote (`token_refused`, `settings_rejected`,
    `agent_suspended`, `owner_changed`, …).
  - `PATCH /agents/{id}`, by merge-patch and only with `If-Match` (428
    without, 412 at another version): the owner's model, from the
    provider offers of `GET /models`, and their key, sealed; the offer of
    the school's plan, `model.school: {offer: <id>}` from `GET /models`'
    `school_key.offers` (`unknown_offer` for another, and
    `school_key_not_offered` where the school offers none), or null to
    take the agent off the plan, the owner's model then being its fallback
    and optional, and an agent on the plan having a model; and
    `tools.writes`, whether the agent's model may act for them in the
    course where its seats allow (§4): `true`, `false`, or `null` for the
    default, which is `true`, audited as `tools.writes` changed; the view
    (`GET`) says `tools.writes` as it stands. A model on a
    key of another provider, a denied model, or a row the registry would
    not run (`registry.Check`, the same path as `Build`, for this one
    row) is refused before anything is written. `POST /keys/test` tries a
    key with one output token, and neither stores nor returns it; its
    hint is audited once it has passed as a key a provider may be sent.
    A key that is a Core token, or holds one anywhere (`ais_` or
    `aisinv_` and a public prefix, as Core makes them), is refused by
    both, and nothing of it is kept.
  - `PUT /agents/{id}/token`: a new token of the same agent, sealed in
    place of the old, whose secret is destroyed with the write; the new
    token then revokes the old in Core (`credential_list`, then
    `credential_revoke` of that credential alone, D7). Core refusing the
    new token (401) means another new token replaced it meanwhile, and
    says nothing of the old one, which is then said to have failed
    (`core_refused`), for its owner to revoke. `POST …/pause` and
    `…/resume` set the row's flag, at the version `If-Match` names when
    it names one (412 when the row was written after it was read).
  - `DELETE /agents/{id}`: the stored token opened, the one secret the
    API ever opens, to revoke itself in Core (unless
    `revoke_token=false`); then the row, its courses and its secrets
    destroyed in one transaction, and the agent's notes, attempts,
    cursors, seats, state and leases purged, its ledger kept. The row is
    deleted only while it holds the token that was revoked, and is at the
    version `If-Match` names when it names one: a new token put in
    meanwhile is revoked in its turn and the row deleted holding it (three
    tries at most), and with `If-Match` the write meanwhile is 412. An
    agent suspended in Core cannot revoke its own tokens: the answer says
    so, and its owner revokes them in AIshie.
- **Hosted agents' models** (D9) are called at the providers' own
  endpoints alone, which the API makes from the provider, an endpoint
  choice, an Azure resource or an AWS region (patterns with no dots),
  and the registry checks again. The worker calls them through
  `internal/netguard`: the dialer resolves the host itself and dials
  only public addresses (never loopback, private, link-local and the
  metadata address, CGNAT, or the other reserved ranges, IPv4-mapped
  forms included, and the IPv4-compatible and IPv4-translated ones
  whole), checks the address again as it connects, and no redirect is
  followed; `check --live` tries a hosted agent's model through it too.
  Behind `EGRESS_PROXY` only the proxy is dialed, and the proxy must
  refuse the same. A hosted agent's model is its owner's text: its calls
  are counted (`llm_calls_total`, `llm_tokens_total`) under the name the
  price table gives it, the model or the glob that prices it, and under
  `other` when none does, so that no owner's text is a metric's label.
- **The audit** (D11): `Server.Audit` records an event in `audit`
  (migration 0005) after the change it is about has committed: who, their
  session, their address, the action, its target, the outcome and a
  detail of ids and hints, never a secret, a name or text anyone wrote. The
  store has no transaction that spans a registry write and an audit row,
  so an event that cannot be recorded is logged at error and counted
  (`aishie_api_audit_failures_total`), and fails nothing. Refused
  assertions are counted, not audited. Housekeeping destroys events older
  than 400 days.
