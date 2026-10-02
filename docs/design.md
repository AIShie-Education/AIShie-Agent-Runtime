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
aishie-runtime check      validate the configuration; with --live, read each agent in Core and show its seats
aishie-runtime migrate    the store's schema (Postgres)
aishie-runtime keys       the sealed secrets: check that each opens, or rewrap them under the current key
aishie-runtime catalogue  fetch Core's GET /v1/tools, print its hash, compare it with a snapshot
aishie-runtime version
```

Agents are configured from YAML (§4), with secrets from the environment or
files, as M1 has it, and, with the store in PostgreSQL, from the registry of
hosted agents that people host by their ids from AIShie-Frontend (§11): the
secret store seals their owners' keys, and the tokens Core issues the
runtime for them, in the runtime's database, and the registry runs them
beside the YAML agents. Every agent the runtime runs, the operator's and the
people's alike, is an agent Core hosts `runtime`, named by its id: the
runtime is issued its token by Core's `agent_runtime` service, with the
runtime's own credential (§11.6), and nobody gives it a token. An agent
Core hosts `mcp` is its owner's tools' to reach, never the runtime's, and
the site's runtime is the only one: there is no self-hosted runtime. The JSON API the front
end calls (§11.4) listens apart, on `API_ADDR`. A module of its own, off
unless the site's administrators turn it on, transcribes the course's
files into their text versions in Core (§12). Another, on by default,
converts every Office and OpenDocument file Core keeps to the PDF the
site previews it as, once (§13). `/status` is the
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
  toolset       which tools a seat's model is offered (§4), and running the model's calls; the files
                of documents and of messages, as the models are given them (§4, Files; §5.3, Attachments)
  doctext       the text of .pptx, .docx, .xlsx and PDF files, read from memory within fixed limits (§4, Files)
  ocr           the text of scans and images, recognized by tesseract (§4, OCR)
  search        the search of a course's materials: terms of English and of Chinese, Japanese and Korean,
                passages, BM25 and excerpts (§4, Search)
  office        Office files converted by LibreOffice, and PDFs cut into ranges of pages (§4, Office files)
  sandbox       how the programs of OCR and the conversions are held: prlimit, a timeout, nothing of the runtime's environment
  config        the YAML, its defaults and precedence, validation
  secrets       secret://, env://, file:// and sealed:// references
  vault         envelope encryption of the secrets kept in the store: a data key per secret,
                wrapped by the key KMS_KEY_ID names (§11.1)
  registry      hosted agents: each row made the agent document YAML would hold, loaded on its
                own, merged with YAML; the watcher that reloads on the registry's changes (§11.2)
  pricing       the versioned price table, and cost
  transcribe    the transcriber: the course's files made text versions in Core, by a model of the school's plan (§12)
  rendition     the renditions worker: every Office file Core keeps made its PDF once, by LibreOffice (§13);
                Core's table of the files converted
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

An answer that searches the course's materials (§4, Search) spends one
`document_list` and one `document_get` per document listed (at most 100)
more, once an answer, at the answers' priority: in a course of 100
documents some 100 calls, where an answer that does not search spends
about k + 2 (Core's `docs/agent-runtime.md` §7.3). An agent answering many
students of such a course with the search can so answer some five a
minute at Core's defaults rather than some seventy, and its other calls
wait behind those; a site whose agents search large courses raises Core's
`RATE_LIMIT_PER_MINUTE` (and `polling.assumed_core_rate_per_min` with it)
to suit. The scope is read again at every answer, not kept across
answers, so that a document withheld from a seat is never searched for
it after Core says so.

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
  come. So are a message's files (`conversation_attachment`): a tutor's
  token reads those of every conversation addressed to it, and the URL it
  gives is a credential; the runtime fetches them itself, for the
  conversation it answers alone (§5.3, Attachments). And
  `conversation_upload_url`, a signed URL for bytes a model cannot send.
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
- `document_file`: one file of a version with a fresh URL, a credential for
  the file. `document_get` gives the model every file of a version, and one
  of them by the runtime's own `file_id` (Files, below), fetched by the
  runtime, which asks `document_file` itself for a fresh URL where one has
  lapsed.
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
- `document_text`, `document_text_*`: a document's text version (Core
  #43), its file transcribed into Markdown. Reading one is `document_get`'s
  (its `version.text`). Writing one or asking for it to be transcribed
  again (`document_text_update`, `document_text_retranscribe`) is the
  course's staff's, in the front end: a model's text would stand in place
  of the file for every reader after. The transcription service's queue,
  file, renew and complete are the site's transcriber's, called with the
  service's own credential, and Core refuses them to anyone else.
- `service_*`: the site's service credentials, issued, listed and revoked by
  its administrators alone; a token issued is a credential in the model's
  text.
- `agent_runtime_*`: the site's agent runtime's own service (AIShie-Core
  #52): who owns an agent and how it is hosted, and its one token issued
  and revoked by its id, which the runtime calls itself, over REST, with
  the service's credential, and Core refuses to any other (`service_only`).
  So are its renditions (`agent_runtime_rendition_claim`, `_file`,
  `_renew`, `_upload_url`, `_complete`), the PDFs it makes of Office files
  (§13), whose URLs and upload tokens are credentials for a course's files.
  `agent_*` covers them; they are named for what they are.
- `document_rendition_retry`: sending a file's failed PDF rendition back to
  be converted again (§13): the site's plumbing, which staff send back from
  the front end where it failed, and a model has nothing to judge it by.
  `conversation_rendition_retry`, a message's file's, is
  `conversation_*`'s.
- `conversation_export`, `conversation_export_*`: exporting conversations
  for audit (AIShie-Core #51), every conversation of the site, a
  department or a course, retracted messages with their text, as files
  whose URLs are credentials for them; the site's and the departments'
  administrators' alone. Core refuses an agent's export whatever role it
  holds (`people_only`), and gives an export's files again
  (`conversation_export_file`) to its maker alone. `conversation_*` covers
  them; they are named for what they are.
- `sso_*`: the site's identity providers for single sign-on, set up,
  changed, switched, removed and tested by the platform's administrators
  alone: a provider's client secret is a credential, and a change decides
  who signs in.

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
file (at most 50 MB, a presigned URL through the egress proxy) and gives it
to the model by what it is, whatever the model; what became of it goes in
the result as `file`: its name, type and size, `given_as` (`file`, `text`
in `file_text`, or `not_given`), `extracted_from` when the text is the
runtime's reading of the file (`ocr` when its OCR recognized it),
`converted_to` when what is given is of what LibreOffice made of it
(Office files, below), and a `note` saying why it was not given or what
the text holds and leaves out. Where Core lists the version's files (A
version's files, below), the record names the file by its `file_id` and
`position` too, and `name` is the file's name; before, the document's
title.

- Text (`text/*`, JSON, Markdown) is its text, to any model.
- An image is a file part where the adapter takes files, and otherwise
  what the runtime's OCR recognizes of it (OCR, below).
- A presentation or a document (PowerPoint's `.pptx`, `.ppt`, `.ppsx`,
  `.potx` and their kinds, OpenDocument's `.odp`; Word's `.docx`, `.doc`
  and their kinds, OpenDocument's `.odt`, RTF) is given as its PDF: the one
  Core keeps of it, its rendition (§13), where Core says it is done, and
  otherwise the one LibreOffice makes of it where the runtime converts
  Office files (`OFFICE_PDF`), for every model (Office files, below); and
  goes the way a PDF does: a file part where the model
  takes files and its provider a PDF of its size, in parts of its pages
  when it has more than a part holds (Reading in parts), a deck's speaker
  notes beside it in `file_text`, which the PDF does not show; and
  otherwise its text, with what OCR reads of the slides that show pictures
  (Office files, below). A workbook is always its text, as below, an
  `.xls` or `.ods` one as LibreOffice converts it to `.xlsx`: a spreadsheet
  reads better as its rows than as pages.
- Without the conversion (`OFFICE_PDF=off`, or LibreOffice not
  installed), a presentation or a document whose rendition Core has done
  is still Core's PDF to a model that takes files, as above; otherwise a
  PowerPoint, Word or Excel file (Office Open XML: `.pptx`,
  `.docx`, `.xlsx`, and their macro-enabled and template forms) is the
  runtime's text of it (`internal/doctext`), to every model; so is a
  workbook, and a Word document to a model that takes no files, with the
  conversion: a deck's slides in the order
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
  Gemini's 1,000 pages, Converse's 4.5 MB), in parts of its pages when it
  has more than a part holds, which the provider's pages bound (Reading in
  parts); past its size, or past its pages where the runtime cuts no PDF,
  or for a model that takes no files, it is the runtime's text of it, page
  by page (`## Page N`), where that reads as text. A PDF that needs a password to open is
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
- An older binary Office file (`.doc`, `.ppt`, `.xls`), and an
  OpenDocument one, is converted as above; without the conversion it is
  not given: the note asks for `.pptx`, `.docx` or `.xlsx`, or a PDF. An
  Office Open XML file encrypted with a password, in the same container, is
  given to no model, and not converted. A file of no type, or of one that
  says nothing (an octet stream, a zip archive), is known by what it
  holds, and, where the runtime converts Office files, an older one by the
  stream its container names, an OpenDocument one by its `mimetype`, RTF by
  its first bytes; anything else is not given, with its type named.

**A version's files** (`toolset.renderVersion`; AIShie-Core #49). A
version of a document is text, files, or both, its files in order, each
named: a lecture's slides, its handout and a sample program. Core lists
them in `version.files` of `document_get`'s result, each with its id,
place, name, type, size, checksum, a URL and its own text version, and
keeps the version's `download_url`, `content_type`, `byte_size`,
`checksum` and `text` as the first file's for the runtimes of before. The
runtime reads every file, not the first alone, and gives the model each
as a version's one file is given (this section and those below), under
its name, in its order: the file's text version first where it is done,
and otherwise the file, fetched, as text, a file part (named as the file,
so that the model tells them apart), or why not. The result keeps Core's
envelope, without its URLs and without the text versions' bodies, which
the runtime gives itself (a body in the envelope would take the room the
text is given in, twice), and gives the files in `files`, each its
record and `file_text`, with `files_note` saying how they are given and
read. What one call gives is bounded as the first turn bounds a
question's files (§5.3, Attachments): each file at most what a result
gives of it, the first part of a long text or of a PDF of more pages than
a part holds, with `next_part` naming the file; all of them at most two
results' worth of text (`versionResults`, 64 KB at the defaults) and one
PDF part's pages of file parts (`PDF_PART_PAGES`, an image counting as
one). A file past that, or one the answer's time ran out for, is named
with the call that reads it (`next_part`), unfetched where it would be a
file part and the pages are spent. `file_id`, a third argument the
runtime adds to `document_get`'s schema and takes out again, as it does
`file_part` and `file_pages`, reads one file of the version alone: its
parts and its pages, each call naming the file again, exactly as a
version of one file is read; an id of no file of the version is said so,
and `file_part` or `file_pages` naming no file of several give the
version's files and say to name one. A version of one file is read as it
always was, whichever Core lists it, and its calls name no file. A URL
Core gave that has lapsed (the file server refuses it) is asked for
again of `document_file`, with the caller's own token, and the file
fetched once more; neither reaches the model, and `document_file` is not
offered to it (§4's list above). A Core before #49 lists no files: the
version's one file is read from its `download_url`, as before.

**Text versions** (`toolset.giveTextVersion`; Core #43). A version of a
course's document with a file may have a text version in Core, beside the
file (`version.text` of `document_get`'s result; since AIShie-Core #49 each
file has its own, `version.files[].text`): the file transcribed into
Markdown by a model, a page under `## 第 N 頁` (a slide under `## 投影片
N`) and its pictures described in brackets, by this runtime's transcriber
(§12) or another's, or what the course's staff wrote or corrected. Where it
is `done`, the model is given it first, in place of the runtime's own
reading of the file, which is then not fetched: `file_text` is the text,
`given_as` `text`, and `text_source` says whose it is, `AI transcription
(<the offer's label>)` or `edited by staff`; the note says a transcription
may hold mistakes. A text too long for one result is given in parts as
any text is (below), cut where its pages begin. Core gives the text whole
beside the version up to 64 KiB, the bodies of a version's files together
(the rest are left out); a longer one is read a part at a time
(`document_text`, with the caller's own token, which reads the text exactly
where it reads the version, naming the file by its `file_id`), all at one
revision, read again once should it change meanwhile; what was read is
kept (`Runner.Texts`) under the file, which never changes (from a Core
before #49, the version), and its revision, and each worker drops it as
Core's text events say the file's text changed (`document.text_updated`,
`…rubric_…`, `…draft_…`, and their `_unreleased` forms, whose payload
names the `file_id` and the `version_id`: both are dropped). A model that takes files may
ask for pages of the file itself to check one against the text:
`file_pages`, the runtime's other argument of `document_get` (`"3"`,
`"3-5"`; at most 10 at a time, and never more than its provider takes in a
file), gives those pages as a PDF of their own, cut from the file or from
its PDF (Core's where its rendition is done, else LibreOffice's), a deck's
speaker notes beside them, and no text; a model that takes no files is
given the text of those pages alone, by their headings (Reading in parts),
and told why not the pages. `file_part` and `file_pages` are not asked for
together. A text version not done (pending, working, failed, skipped), or
none, changes nothing: the file is given as above. All of this holds
whether or not this runtime transcribes.

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
2}}`, and `file_id` for a file of a version of several), which its `note`
says in words; the first part says how many there
are, and, when there are at most twenty, what each of the others holds,
so that a model looking for one slide asks for its part at once. A model
given a file's text (one that takes no files, or a file whose pages are
not given as a PDF) may also ask for slides, pages or sheets of it by
`file_pages` (`toolset.pagesOfText`): their text alone, from where the
first begins to where the next after the last does, `part_holds` naming
them, where it fits a part; else the part that holds their start; the
note says why not the pages themselves, and a text with no such sections,
or not those, is given as it would be, the note saying why. This is what
a search's hit is read by (Search, The pointer), since a model's parts are
not the same at every runtime.

Parts are numbered, not cut by slide or by byte offset: a slide may be
longer than a part, and a range of slides the model chose could be again
too long for one result (`file_pages` then gives the part that holds its
start), while a part always fits one and the first says how many there
are. The model asks with `file_part`, an argument the
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
says which there are; `file_part` of a file given whole as a file part
(an image, a PDF of no more pages than a part holds) is noted and does
nothing. Only when
Core's own result leaves a part too little room (a document whose own
text, `body_md`, is kilobytes long besides its file) is the part cut, and
the note says so.

A PDF given as a file part is its pages as pictures and text to the
model's provider: a slide of a lecture some two thousand input tokens,
sent again at every later turn of the answer (a deck of 38 slides, near
100,000 of an answer's 150,000). So a PDF, the course's own or LibreOffice's of a deck
or a document, of more pages than a part holds (`PDF_PART_PAGES`, 10, and
never more than its provider takes in one file) is given in parts of its
pages (`toolset.givePDFFile`): part k is pages (k−1)·10+1 to k·10, a PDF
of its own, which poppler's `pdftocairo` draws again, text as text,
pictures as they were, each font once (`pdfseparate` and `pdfunite` would
put every font of the file in every page: ten slides of a lecture, eleven
times the whole deck), kept in the worker's memory with the conversions
(Office files, below). Its record is a text part's: `part`, `parts`,
`part_holds` (`slides 11–20`), `next_part`, the first part saying what the
others hold, and a deck's speaker notes of those slides alone in
`file_text`. A PDF past its provider's pages, which was its text, is now a
file in parts of them where the runtime cuts PDFs; one past its provider's
size is still its text, and one of pages that could not be cut is its
text, saying so. Where poppler's programs are not installed, a PDF is
given whole, as before.

What was read of a file is kept per worker (`toolset.TextCache`), so that
the file is fetched and read once, not once a part: keyed by the version
Core named and the file of it (by its id, where Core lists files), its
checksum, and the limits it was read within, and kept
only for a model given the text (a file part needs its bytes, which are
never kept). A version's file never changes, and the cache is reached
only after Core has given the caller that version, so every agent of the
worker shares it. It holds at most 32 MiB of text in all, the reading
used least recently going first past that; a reading that ran out of time
is not kept, and neither is one larger than the whole bound. A file a
message of a conversation carries is kept by its checksum, and tagged with
its id and its message's, so that it goes when the message is retracted
(§5.3, Attachments).

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
  of the worker, never in it (package `sandbox`, which LibreOffice runs
  under too), and is held from its first instruction by
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
- *The site's setting* (§11.5). Where the environment lets OCR run, the
  runtime's administrators turn it off and on and choose its languages
  among those tesseract lists (`--list-langs` as the engine starts, but
  `osd`), `OCR_LANGUAGES` by default; the environment is the ceiling, and
  with `OCR=off` or the programs missing the setting changes nothing.
  Each worker puts it in force as it reads the registry again
  (`ocr.Service.Set`): off, no file is recognized and no kept text read,
  as with `OCR=off`; in other languages, a text kept of a file (its
  `engine` names the languages it was recognized in) or held in the
  `TextCache` (under the checksum and the languages) is not given, and
  the file is recognized again, in them, and kept in its place. A job
  already running finishes in the languages it began in. Languages a
  worker has not got (another image) are logged there, once, and change
  nothing on it.
- *Counted*: `ocr_requests_total{result}` (a question's outcome: `done`,
  `failed`, `in_progress`, `started`, `busy`, `off`),
  `ocr_jobs_total{kind,outcome}` (`done`, `empty`, `too_large`, `timeout`,
  `failed`, `cancelled`), `ocr_pages_total{kind,outcome}` (`text`, `empty`,
  `failed`), `ocr_job_seconds{kind}`, `ocr_page_seconds{step}` (`render`,
  `recognize`), and the gauges `ocr_jobs_running` and `ocr_jobs_waiting`;
  and one log line a file, with the start of its checksum, its kind, the
  outcome, pages, characters and time, never its text.

**Office files** (`internal/office`, `toolset.giveConverted`). The
runtime's text of a deck leaves out what a lecture's slides are much made
of, screenshots of code, diagrams, charts and formulas, which it only
names (`[image]`, `[chart: title]`), and it read no older Office file at
all. So a presentation or a document is converted to PDF by LibreOffice,
and a model that takes files sees its pages as they look.

- *What converts, to what* (`office.Converter`). `.pptx`, `.ppt`, `.ppsx`,
  `.potx`, `.odp` and their kinds, and `.docx`, `.doc`, `.odt`, `.rtf` and
  their kinds, to PDF (`soffice --headless --convert-to pdf`): a page a
  slide, hidden slides too, so that its pages are numbered as the runtime's
  text numbers the slides, and no notes pages; at most `OFFICE_PDF_MAX_PAGES`
  (300) pages, the note saying the file may have more; its pictures
  brought down to 300 dpi where they have more. `.ppt` and `.odp` to
  `.pptx` as well, whose slides and notes `doctext` reads; `.xls` and `.ods`
  to `.xlsx`, read as any workbook. A file that is not held as an Office
  file is (a zip, a Compound File Binary file, RTF) never reaches
  LibreOffice, which would take it for text or a web page and make a PDF
  of that.
- *Core's PDF first* (`toolset.pdfOf`, `internal/toolset/rendition.go`).
  Core keeps one PDF of every Office and OpenDocument file, made once for
  the whole site (§13), the one people see in the viewer. Where
  `document_get`, `document_file` or `conversation_attachment` say a
  file's `rendition.state` is `done`, the PDF of it the runtime gives a
  model, cuts into parts, and reads text and OCR from where it reads them
  from a PDF, is that one: fetched
  by the runtime, never the model, from `rendition.download_url`, a URL
  good for some 15 minutes that is a credential for the PDF, with no
  credential of the runtime's, at most `MaxFileBytes` (50 MiB; one Core
  says is larger is not fetched), and kept by the file's checksum as
  LibreOffice's PDF is (`office.Service.TakeRendition`, below), so that it
  is fetched once a worker, and a conversion of the file after finds it.
  Its pages are counted, and past `OFFICE_PDF_MAX_PAGES` it is cut to its
  first so many, as LibreOffice's is made, and said so. A URL that has
  lapsed, by its `download_expires_at` (30 s before it) or by the file
  server's refusal (403, 404, 410), is asked of Core again, once, with the
  caller's own token (`document_file` for a version's file named by its
  id, `conversation_attachment` for a message's), and taken only for the
  same file (version, checksum). The deck's speaker notes are read from
  the deck as before. Where Core has no PDF to give (the rendition queued
  or claimed, failed or skipped, none for the file, a Core from before
  renditions), or its PDF cannot be had (not fetched, not a PDF that
  reads, past the pages with nothing to cut it), the runtime converts the
  file itself, as below. A PDF of Core's not had is tried once a call
  (not again for the document's text read from its PDF, or OCR's slides
  picked from it), and one not taken is remembered by the file's checksum
  for five minutes (`office.RenditionRetention`; but for a fetch the
  question's end cancelled), and not fetched again meanwhile, so that
  where nothing here converts the file, a PDF that cannot be had is not
  fetched, up to 50 MiB, at every question. The URL reaches no model (Core's result is
  stripped of every `download_url`) and no log: what the fetch said is
  dropped, leaving the server's status or that it was too large, and the
  conversions log a PDF taken or not by the start of the file's checksum
  and an outcome (`taken`, `not_fetched`, `not_a_pdf`, `too_large`,
  `too_many_pages`, `not_cut`, `cancelled`). Where LibreOffice does not
  convert here at all, a presentation or a document whose rendition is
  done is given to a model that takes files as Core's PDF all the same
  (`toolset.Runner.rendered`), its notes beside it where the runtime reads
  them itself (an Office Open XML deck), and is otherwise what it was
  without the conversion.
- *To a model that takes files*: the file's PDF, Core's or LibreOffice's,
  as a PDF is given (its size held to its provider's, in parts of its
  pages), `converted_to: "pdf"`, the note saying what it is; a deck's
  speaker notes of the slides given in `file_text`, each `## Slide N` and
  `Notes:` as the runtime's text has them. A deck of `.ppt` or `.odp` has
  its notes from its `.pptx`.
- *To a model that takes none*, or past its provider's size: a deck is the
  runtime's text of its PowerPoint form (the file's own, or LibreOffice's),
  which keeps titles, bullets, tables and notes as a PDF's text would not
  (it has the master's footer on every page, a table as runs of words); and
  the slides that show pictures or charts (`doctext.Result.Slides`, at
  most 40) are picked out of its PDF (`pdfseparate`, `pdfunite`) and read by
  OCR, once, in the background, as a scan is, under a checksum of the
  runtime's own made from the deck's (`derivedSum`), so that every copy of
  the deck and every conversion of it is read once. What OCR read of each
  slide follows the slide's own text, before its notes, after `[OCR of the
  slide as drawn]`, and the note says it may hold recognition errors and
  holds the slide's own text again. While OCR reads them the slides are
  given without it, `ocr: "in_progress"` and `ask_again` saying to ask
  again; with no OCR here, the note says the text in its pictures is not
  read. A Word document is the runtime's text of it, as before, its
  headings, lists and tables kept, unless it is little but pictures (under
  200 letters besides them: scanned pages, as a school's often are);
  that one, and another document (`.doc`, `.odt`, `.rtf`), is the
  runtime's text of its PDF (Core's where its rendition is done, else
  LibreOffice's), page by page, and where that
  has no text to read, what OCR recognizes of the PDF, under a checksum of
  the runtime's own.
- *In the background, once* (`office.Service`, the worker's). One file is
  converted at a time, eight wait (each holds its bytes), and past them a
  file is not started (`busy`). A file is known by its checksum, and each
  conversion of it is made once for every agent of the worker; a question
  waits for it at most `OFFICE_PDF_TIMEOUT` (2 min), never past half the
  answer's time left. Most take seconds (a lecture of 38 slides and 4.7 MB,
  4.6 s; LibreOffice's start, 1.5 s of it); one that takes longer is left
  to its job, and meanwhile a PowerPoint or Word file is given as its
  text, `conversion: "in_progress"` and `ask_again` saying to call again in
  a minute to see its pages, and a file read only through LibreOffice is
  not given yet, and says so. A file LibreOffice cannot open (damaged,
  protected by a password, not what it says it is) or convert in time is
  said so (`conversion: "failed"`): a PowerPoint or Word file is still its
  text, another not given.
- *Kept in memory.* What LibreOffice made, the PDFs fetched of Core, and
  the ranges of pages cut from them (those of Core's PDF apart from
  LibreOffice's), are kept in the worker's memory by the file's checksum (64 MiB
  in all, the least recently used going first), and a failure for an hour;
  not in the store, as OCR's text is: a PDF is megabytes, which PostgreSQL
  would keep, back up and replicate for every deck, while making it again
  takes seconds, once a worker; nor on disk, which the container may not
  keep or have room on. The text read of it is kept in `TextCache`, as any.
- *Held in.* LibreOffice runs as OCR's programs do (package `sandbox`,
  which both use): prlimit's limits from its first instruction (2 GiB of
  address space, `OFFICE_PDF_MEMORY_MB`; twice its time in CPU, for its
  threads; 256 MB a file; 1,024 open files, for its fonts and libraries;
  no core dump), a hard timeout whose end kills its process group, niceness
  10, nothing of the runtime's environment (`PATH`, `LC_ALL=C`, `HOME` and
  `TMPDIR` in its private directory, `SAL_USE_VCLPLUGIN=svp` for drawing
  with no display), the directory removed whatever happens. It starts each
  time from a fresh profile made there, whose settings keep a hostile file
  from reaching outside itself: every link out of a document is blocked
  (`BlockUntrustedRefererLinks`; LibreOffice otherwise fetches a picture a
  document links on a web server as it opens it, which a test saw it do),
  its proxy is one nothing listens at, should anything try, and active
  content (OLE objects, DDE links) and macros are off; a section linked to
  a local file keeps the text the document holds, a conversion answering
  no to LibreOffice's asking whether to update it. The integration tests
  hold documents to each: a picture on a server of the test's is never
  asked for, a secret in a local file never reaches the PDF. There is no
  network namespace, as for OCR.
- *Off.* `OFFICE_PDF=auto` (the default) is on where `soffice` and prlimit
  are installed, as they are in the image, and off, with a warning at the
  start, where they are not; `OFFICE_PDF=on` refuses to start (and `check`
  fails) without them; `OFFICE_PDF=off` is off. Off, files are given as
  they were before it: a PowerPoint, Word or Excel file as the runtime's
  text, an older or OpenDocument one not at all; but for one whose PDF
  Core made, which a model that takes files is given (Core's PDF first,
  above). PDFs are cut into parts
  wherever poppler's programs are, whatever `OFFICE_PDF` says. The start's
  log line and `check` say which.
- *Counted*: `office_requests_total{result}` (`cached`, `failed`,
  `started`, `in_progress`, `busy`, `off`; and `rendition`, a PDF of
  Core's fetched, `rendition_failed`, one not taken, or remembered as not
  taken),
  `office_conversions_total{to,outcome}` (`done`, `failed`, `timeout`,
  `too_large`, `cancelled`), `office_conversion_seconds{to}`,
  `pdf_cuts_total{op,outcome}` and `pdf_cut_seconds{op}` (`range`: a
  file part's pages; `pick`: the slides OCR reads), and the gauges
  `office_conversions_running` and `office_conversions_waiting`; one log
  line a conversion, with the start of the file's checksum, its format
  and target, the outcome, pages, bytes and time.

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

**Search** (`toolset.SearchTool`, `internal/search`; AIShie-Agent-Runtime
#39). A model looking for something in a course listed its documents by
title and read them whole, a part at a time: a deck of 38 slides is three
parts, and a title need not say where a passage is. So the runtime offers
a tool of its own, `course_materials_search {query, limit, page}`, which
gives the passages of the course's documents that best match a few words:
each hit the document (its id, title and kind), the version and the file
(its id and name), the slide, page or sheet it is on (`where`), whose text
it is (`text_source`), an excerpt of at most 200 characters around the
words, and `read`, the `document_get` call that gives the passage itself,
which the model makes as it is (and `read_note` where it gives the file
from its start instead, and `passage`, the passage itself, where it would
give it cut short: The pointer, below). A page holds 5 hits (`limit`, at most 10);
`more` and `next` give the next page. The result says how many documents
and files were searched, how many files have no text the search can read,
and how many were not read yet; a search that finds nothing says what it
could not search, so that the model does not take "not found" for "the
course never says it".

- *Offered* (`Set.WithSearch`) to every seat's model whose set offers
  `document_list` and `document_get`, through which it reads (a seat that
  reads the material or the rubrics), in every conversation, as a read:
  not where `tools.deny` names it (or a `*` entry covers it, `course_*`),
  nor in mode `none`; Core's catalogue offering a tool of its name fails
  `Check`. The prompt (§6) says what it does, and to read a hit before
  relying on it. The answer's draft shows it as listing documents.
- *Only what the seat may read.* The index is the course's, shared by
  every agent and seat in it, and holds nobody's permission. Every search
  asks Core, with the asking seat's own token, what it may read, by the
  very calls a model reads with: `document_list` (the first 100 documents
  it lists, by their ids, which are made in time order, so the oldest
  first, asked for 101, since Core names a next page whenever a page is
  full, and a course of exactly 100 has none more; archived ones aside:
  what Core lists the seat,
  its drafts only to a seat that reads drafts, instructions and rubrics
  only as their assignments are released to it, no submission or feedback
  file), and `document_get` of each, of no version, which gives the
  version the seat reads (the published one, or the latest to a seat that
  reads drafts) and its files, each with its text version's status and
  revision. Those files, each at that revision, are all the store
  searches: a passage of another version (a draft, a newer version than
  the published one), of a document Core does not give the seat now, or
  of another revision of a file's text is never a candidate, whoever read
  it into the index. The result says which documents listed were not
  searched, and why: those Core gives the seat no version of now (the
  version or the document purged, none it may read, or the document
  refused it, withheld since it was listed), which a search again will not
  find, apart from those Core could not be asked of just now (not reached,
  or its answer not read), which the model is told to search again for in
  a minute. What one answer's seat may read is read at its first search
  and used by its later ones (`toolset.SearchScope`): once an answer, a
  list and a `document_get` a document, at most four at once, within the
  agent's rate limit like any call, which in a course of 100 documents is
  some 100 calls an answer that searches (§2.2).
- *What it searches*: the text a model given the file as text reads, from
  the same pipeline (`giveFile`): Core's text version where it is done
  (staff's or an AI transcription), otherwise the runtime's own reading of
  the file (a text file, a PDF's text, an Office Open XML file's), and the
  version's own text (`body_md`). A search starts neither OCR nor
  LibreOffice, and fetches no PDF of Core's (§13): a scanned file, an
  older Office file or an OpenDocument one
  is searchable once its text version is done, which the transcriber
  (§12) makes where the site's administrators have turned it on; without
  it such a file is never searchable, though a model that reads it is
  given OCR's or LibreOffice's text, and the result counts it among the
  files with no text to search. A text is cut into passages
  (`search.Chunks`): one a slide, page or sheet, a longer one cut at a
  paragraph or a line near 1,500 bytes, and cut again where a part of the
  text, as `document_get` gives it (Reading in parts), begins inside it,
  so that a passage is in one part.
- *The index* (`store.SearchIndex`, migration 0014: `search_file` and
  `search_passage`), per course and version, by the file's key in its
  version (its id, or `body`) and the revision of its text, each naming
  `<reading>`, how the runtime reads a file and cuts a text, which a
  change to either moves on: `text:<reading>:<n>`, the text version's,
  which every edit moves on; `file:<reading>:<checksum>`, the runtime's
  reading of a file, which never changes; `body:<reading>:<sum>`. It is
  built lazily: a search reads the files of its scope that the index lacks
  at the revision the seat was shown, at most four at once, within 20 s and
  half the answer's time left, and keeps each as it is read, a file with no
  text kept with no passages, so that it is not read at every search; a
  file that could not be read now (not fetched, read too slowly, its text
  version not given) is not kept, and one the time ran out for is left for
  a later search, which the result says. A text edited since, or a text
  version done since, is read again in its place. What the runtime read is
  kept apart from what it gives models (`TextCache`), which with OCR and
  LibreOffice may be other. A search marks the files it reads as needed,
  for retention, where they were last marked a day or more before
  (`store.SearchUseGrain`), so that it does not rewrite every row of its
  scope.
- *Dropped* when a version or a document is purged: as the worker reads
  Core's `document.purged` (a seat that reads drafts sees it) and
  `document.purged_unreleased` (one that writes assignments, of
  instructions or a rubric not yet released), naming the version, or else
  the document, every version of it; as a search's `document_get` gives a
  version's tombstone; and by housekeeping, a file no search has needed
  for 30 days (`toolset.SearchRetention`), which bounds how long a purge
  no worker heard of leaves its text, and an archived document's. A whole
  document purged is archived, which `document_list` lists to no search,
  so its text leaves by the event or by retention alone.
- *Terms and ranking* (`internal/search`). PostgreSQL's full-text search
  keeps a run of Chinese as one word, so 排序 is not found in
  合併排序的複雜度, and pg_trgm's trigrams depend on the cluster's
  character classes: under a C ctype it finds no word in Chinese at all.
  The runtime makes the terms itself, the same in either store and under
  any locale: a text folded (NFKC, lower case), its words of the
  alphabetic scripts with their plural endings taken off, and of Han,
  kana and Hangul every character and every pair side by side, the
  bigrams of Lucene's CJK analyser. A query of several characters is
  matched by its pairs, one of one character by the character; English
  stopwords go where other words are left. A passage keeps its terms once
  each in a `text[]` under a GIN index, which every PostgreSQL has (13 to
  18; no extension, so nothing for the deploy's `postgres:18` to install,
  and a test runs it under a C locale); the store gives the 400 passages
  of the scope that hold the most of the query's terms, of those that hold
  as many the shortest (BM25 scores a term held as often the higher in a
  shorter passage; terms are kept once each, so how often is not known
  there), then by version and place, with how many passages hold each
  term and how long they are, and the runtime scores them by BM25, times
  how many of the terms each holds, half again where it holds the query
  as written, and orders ties by the course's order: the documents'
  `sort_order`, as staff set it, then the oldest first.
  Simplified and traditional characters are not taken for each other: a
  query is matched in the script it is written in. Embeddings may come
  later, in the same index.
- *The pointer* (`read`) is the call that gives the passage as the
  asking model reads the file, which need not be as the index read it:
  a model that takes files is given a PDF, a deck or a document as its
  pages, and on a runtime with OCR and LibreOffice a model that takes none
  is given a deck with what OCR read of its pictures after each slide,
  which moves where its parts begin. So: of a text version, which every
  model reads as the index did, the part of it the passage is in
  (`file_part`, as `splitText` cuts it, which the index records with each
  passage); of the runtime's reading of a file, a passage on a page or a
  slide by that page or slide (`file_pages`), which gives the page or
  slide itself to a model given the file's pages, and that page's or
  slide's text alone to one given its text (Reading in parts); one on
  neither (a text file's, a Word file's, a sheet's), whose text every
  runtime reads as the index does, by its part; but in a Word file given
  as its PDF (Core's rendition of it, or LibreOffice's), whose pages the
  index does not know, the hit says so (`read_note`), and `read` gives the
  file from its first pages. Which a model is given is what `document_get`
  gives it now: a deck or a Word file whose PDF Core made is given as its
  pages even where LibreOffice does not convert here. (A Word file of little but pictures, which a runtime with
  LibreOffice reads from its PDF, is pointed at its first part, which
  holds all the few words the index has of it.) Of the version's own
  text, the version. A version of several files names the file
  (`file_id`).
  `document_get` gives a result of at most `MaxResultBytes` (32 KiB),
  the version's own text in its envelope, and cuts the envelope as one
  string where it passes that: the version's own text far into a long
  `body_md` is not given, and a long `body_md` (or a long list of files)
  leaves a file's part too little room beside it. As it reads a version, the search reckons how
  much of its own text that call gives (`readRoom`, keeping 2 KiB for
  what the runtime adds, so that it may say less is given than is, never
  more), and whether a file's part fits beside it whole; a hit the call
  would cut short gives the passage itself (`passage`, about 1,500 bytes)
  with a `read_note` saying why, beside the same `read`. A passage given
  as pages of a file is given whatever the envelope, and gets none.
- *Who wrote it does not weigh.* Staff's text, an AI transcription and the
  runtime's reading of a file are ranked by how well they match alone: a
  file has one text at a time (the text version where it is done, which a
  staff edit replaces), so no passage is found twice by its sources, and
  a transcription's mistakes are misreadings, not a passage less about
  the question, while it is often a scanned file's only text. The hit says
  whose the text is, and the note that an AI transcription may hold
  mistakes; ties break by the course's order (`sort_order`, then the
  oldest first).
- *Counted*: `search_requests_total{result}` (`hits`, `none`, `refused`,
  `unavailable`) and `search_files_total{outcome}` (`text`, `empty`,
  `failed`, `not_yet`); one log line a search, with its counts and time,
  never its query.

*Where the index belongs.* It could be Core's, as `document.search`,
which MCP agents and the front end would have too, and which would check
the reader's permissions in its own query instead of a `document_get` a
document. It is the runtime's for now: much of what it searches is the
runtime's own reading of files that have no text version yet, which Core
does not have; and it changes no API and moves no pin. Once Core's text
versions cover every file of a course's material, the index belongs in
Core: the runtime's tool then calls it, gated as `document_get` is, and
the runtime's tables are dropped by a migration. That is a Core issue of
its own.

Every write sent is recorded, in ids, counts and codes, never its
arguments: in the answer's ledger row (the writes sent, and how many Core
executed, proposed, denied and failed), in `tool_writes_total{tool,
outcome}` (those four, `error`, `unreachable`, and `refused` for one past
the budget or one kept off a seat), and in one log line each, with its tool, number, key, status,
error code and action, which with Core's own action log is the audit of
what the agent did. An executed or proposed write is noted in the
conversation's memory. A proposal of a model's write is its owner's to
follow in Core: the events poller settles only the runtime's own answers,
and the closes an earlier version proposed, and such a proposal does not
hold the actions cursor back.

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
same agent in Core (`core.agent_id`, which the configuration also refuses
twice) goes to state `error`, naming the first, and is issued no token.
`SIGHUP` reloads the configuration and the price table:
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
agent's row dropping its token (its owner asking for a new one, §11.4) is
a change of its own, which starts it again, to be issued another. The
worker's own write of the token it was issued is not: it puts the row as
it wrote it in force for the agent at once, so that the rebuild the write
sets off restarts nothing. `SIGHUP` goes through `Reload`, which starts
every such agent again, an operator's agent being issued a new token. A hosted agent that does
not pass is not run, and is shown in state `error` with every problem; it
keeps none of the others from running.

An agent's token is the one Core issued the runtime for it, and `me_get`
must still name the agent its configuration does, or it is stopped in
state `error`. And the operator's configuration wins an agent in Core: a
hosted agent on the agent a YAML agent names (`core.agent_id`) is not run
(`operator_agent`, refused by the registry and, should the two meet in a
worker, stopped in state `error` whichever started first, until a
reload); the API refuses to host one.

A hosted agent runs only for its owner (the product owner's D5), and only
while Core hosts it `runtime` (AIShie-Core #52, §11.6). As the worker
holding its lease starts an agent, before any call as the agent, it reads
the agent as Core hosts it, with the runtime's own credential
(`agent_runtime.agent`): an `mcp` agent is not run (state `error`, reason
`mcp_agent`, until its configuration changes), nor an id Core has no agent
of (`agent_not_found`), nor any agent against a Core with no such service
(`core_too_old`). A hosted agent's owner in Core must be the owner its row
names, the person who hosted it: an agent's owner never changes in Core
now, but a row from before may name another (an agent given away while
Core let it be, or hosted with a token its holder did not own); such an
agent is stopped in state `owner_changed`, whose detail names no one, and
its token is revoked in Core (`agent_runtime.revoke_token`) and dropped
from its row; it stays stopped, making no call, until its row changes or a
reload. An agent suspended in Core is stopped (`agent_suspended`) and its
token revoked: its hosting has ended, and it is hosted again, issued
another, by itself once it is reactivated, the start being tried again
after a backoff. One whose owner is suspended keeps its token, Core
pausing it while its owner is, and is not started (`owner_suspended`)
until they are reactivated. When the check passes on a row not yet marked
`owner_verified` (one stored while holding a pasted token was the proof),
the worker marks it, at the version it read. YAML agents' owners are not
checked: their owner is whoever their operator says.

Then the agent runs with the token the runtime holds for it (§11.6): a
hosted agent's, issued to the runtime and sealed in its row; an operator's
agent's, sealed in the store (`agent_token`, migration 0013) when the
runtime has a keyring, else held in the worker's memory. Holding none, the
worker is issued one by the agent's id (`agent_runtime.issue_token`, under
a fresh key each time), which revokes the one before, seals it and keeps it
where every worker finds it, then runs the agent. A hosted agent's row
holding a token its owner pasted before hosting was by id is issued one in
its place at its next start, which revokes the pasted one. Only the worker
holding the agent's lease is issued its token, and it keeps it only while
the row (or the store's token) is still as it read it: a token another
worker kept since that this one did not replace is run with instead, and
this one dropped (Core revoked it as the other was issued); a row paused
or deleted meanwhile has the token just issued revoked. So two workers
sharing a store never each hold one, revoking each other's.

An agent (`worker.Agent`) then starts with `me_get` (the token works), the
catalogue, and `me_memberships`. People in the site may ask an agent Core
hosts `runtime` while the token the runtime was issued for it is live, it
and its owner active: nothing is declared (the runtime no longer calls
`me_site_chat`), and the token's revocation stops the asking.

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

A 401 anywhere stops the agent (state `unauthorized`): the token the
runtime was issued was revoked in Core, by the agent's owner, an
administrator or a migration. So does an MCP envelope with status `error`
and code `unauthenticated`, which the real Core sends where REST answers
401 when the token's actor no longer exists. A hosted agent stays stopped,
reason `token_refused` (the API's `needs_token`), until its owner asks for
a new token (`POST /agents/{id}/token`, which drops the row's, and the
worker is issued another); an operator's agent's token is forgotten, and a
reload is issued another. An instance whose configuration was replaced
while it wound down (a hosted agent's new token put in force before the
old instance's answer in progress ended) records nothing as it ends: its
401 is most often the old token's, which the new one revoked, and the agent
starts on its new token at the next lease tick. The runtime's own
credential is read at each call of the service, so a new one in its place
is taken with no restart; a model key is read when an agent starts, so a
rotated one takes effect at the next start. A paused agent makes no calls
at all; an operator's agent paused or removed in the configuration has its
token revoked in Core by the worker holding its lease, before the lease
goes.

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
  window is settled within milliseconds. So are they, read at once and then
  long-polled, while one of the seat's answers is being written (§5.3's
  step 7), for a retraction of its question, which stops it (§5.3, A
  question withdrawn). The background read every `events_s` stays at
  45 s: the inbox's long poll sees the decisions that put a question back
  (rejected, cancelled, failed), but only the feed tells a proposal
  approved and posted after the window, and a message retracted, whose
  memory is to be forgotten promptly (§6.3). The cursor
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
   without posting. Past `max_attempts`: skip it until tomorrow
   (`on_attempts_exhausted: skip`), X left open. The runtime closes no
   conversation, where §2.4 closes X (`close:{X}`): nothing ends a
   conversation in the product any more. A configuration written before
   that still says `close` is taken, and done as `skip`; its
   `prompt.close_reason_text` is taken, and unused. Each is logged as
   deprecated when the configuration is put in force, and `check` shows
   it. A close an earlier version wrote ahead and left `sending`, or
   proposed and left waiting for a person, is still settled as it was
   (§5.4).
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
   the opener's newest message is retracted, the question is withdrawn
   (below): it is not answered, and the pass is recorded `dropped`. If it
   is no longer M, answer that one instead.
6. **Prompt**: the system prompt (§6 below), the seat's facts, X's memory,
   and the history as turns: the opener's messages as `user`, the agent's as
   `assistant`, retracted ones as `[message retracted]`.
7. **Loop** (§7.1) with the seat's toolset, its writes only when the
   owner opened X (§4), bounded by `per_answer`; the writes, by
   `max_writes`, each keyed for this attempt. Stop
   `end` gives the body. `max_tokens` with text is continued, not written
   again (below); with none, its thinking or a tool call it did not finish
   having taken the cap, the turn is tried once more with twice the cap.
   `content_filter` and `refusal` give `on_refusal_text`;
   `context_overflow` halves the history and tries once more; `tool_error`
   retries the turn once. A spent budget takes a last turn with
   ForceAnswer, and gives `on_budget_text` if that has no text.
   `turns` is a hard cap: the last call it allows is the forced one, and a
   provider's error is not a turn, nor a continuation (below). The output-token budget forces the last
   turn as soon as what is left cannot hold a whole one, and caps it at what
   is left; tool calls past their budget get an error result and never reach
   Core. A last turn forced by the wall clock gets min(wall clock / 6, 15 s)
   more; the claim's Core calls stop at the wall clock plus 25 s, inside the
   lease. A call is given the wall clock left, at most 120 s: §7.1's 60 s
   was for a cap of 2,000 tokens, and a whole cap of the default 4,000 at a
   slow provider's pace takes about two minutes, where a timeout would lose
   the answer. A provider that cannot be reached is tried up to three times
   with backoff within the wall clock (honouring `Retry-After`), then the
   fallback model, which an auth or bad-request error also moves to. While a
   fallback remains, a call gets two thirds of the time left, and one that
   times out moves to the fallback at once, so that a provider that hangs
   leaves its fallback time to answer. If all fail, nothing is posted and
   X is held back for a minute, doubling to ten; after five such failures
   on M (counted in memory), `on_budget_text` is posted.

   An answer the output cap of its call cuts off (`max_tokens` with text,
   on any turn, a forced one too) is continued where it stops
   (`worker/continue.go`): the model is given the answer so far as its own
   message and the runtime's word to go on from exactly there, repeating
   nothing (`prompt.Continue`), with no tools; what it writes is joined to
   the answer, less any of the answer's end it writes again, and so on
   while the cap cuts it off. That is one way for every adapter: an
   assistant prefill, where an API has one, is refused by the models that
   think. A continuation is a model call of its own in the ledger but not a
   turn, as turns bound the rounds of tool calls before the answer; it
   writes within what is left of the output tokens, starts only before the
   wall clock is spent and while the input tokens are not, and adds to an
   answer held to `max_body_chars`, less `on_truncated_text`. Its room is
   the least of what is left of those, the wall clock's and the body's at
   the pace, and the characters a token, the answer has been written at so
   far. One whose room cannot hold a whole cap, or whose input spends the
   input tokens, is the last: it is told its room, to close the answer
   within it, and else to end by saying, in the answer's language, that it
   was cut short and that the asker can reply "continue" for the rest. Like
   a forced turn, it is given min(wall clock / 6, 15 s) if the wall clock
   runs out while it writes. An answer still cut off with no room left, or
   whose continuation fails, is posted as the model's with
   `on_truncated_text` in a paragraph after it: its text cut, and a code
   block it leaves open closed, so that the note fits and reads as one. It
   is counted in `budget_exhausted_total{budget="truncated"}`, and the
   answer's log line gives its `continuations` and whether it was
   `truncated`.
8. **Safety** (`safety.Body`, §7 below): links and images whose URLs carry
   context stripped, cut to `max_body_chars` on a paragraph or sentence.
   The model's answer so cut ends with `on_truncated_text`, as one cut
   short at the output cap does (step 7), and is counted with them.
9. **Post**, written ahead: the attempt is stored (`sending`, the exact
   bytes) before `conversation_answer`, and finished with what came back.
10. **Outcome** (§2.4, `worker.Classify`):

| Envelope | Done |
|---|---|
| `executed` (any review state) | record; course hot; memory note `answered` |
| `proposed` | keep the action id (state `proposed`); events polled now, +5, +15, +45 s, or long-polled for those 45 s (§5.2) |
| `denied` | hold the seat until `me_memberships` changes; agent detail says so |
| `failed conflict moved_on` | back to 5 for `details.latest_opener_message_id` (at most 3 times per claim); naming no message, the question was withdrawn: as `idempotency_conflict`, which finds it so |
| `failed conflict already_answered`, `answer_pending` | leave it |
| `failed conflict closed` | drop it |
| `failed forbidden not_addressable` | drop it; read memberships again |
| `failed invalid_argument` | if safety had cut or stripped the body, written again once, shorter and without links, under the next attempt; else failed |
| `error idempotency_conflict` | never resend under that key; next attempt if X still waits on M and M is not retracted (its newest messages, `conversation_messages`), the newer message if the opener wrote again, else nothing |
| `error not_found` | drop it |
| replayed | treated as its stored status |
| replayed `rejected`, `cancelled` | next attempt, with the reason in the prompt |

11. **Ledger**: a row per model call and one per answer, with the writes
    the model made counted by what Core said of them; metrics; release the
    lease.

Every claimed message ends answered, proposed, or with a recorded outcome.

**A question withdrawn.** The opener withdraws what they asked ("stop" in
the chat) by retracting their newest message, and from then nothing waits
for an answer in X until they write again. A Core since AIShie-Core #42
says so: X is `answered`, its draft is deleted with the retraction, and an
answer to M is refused as `moved_on` naming no message, whether it is
proposed, approved or posted. A Core before it (b0eb848 and older) still
says `awaiting_answer`, keeps the draft and would post the answer; the inbox
leaves X out on both. So the runtime goes by the messages, which show M
retracted on both: step 5 answers no question whose newest message is
retracted, and after an attempt that posted nothing it reads X's ten
newest messages (`conversation_messages`), not its state alone, before
trying again.

The answer being written when M is withdrawn stops at once
(`worker/withdraw.go`): its model call is cancelled, so that a provider
streaming it stops too, and the tool calls it is making with it; nothing
more of its draft is written, not even `done`, since Core deletes the
draft with the retraction (the pinned Core keeps it until it goes stale,
120 s); nothing is posted, and nothing more is tried at M. The pass is
recorded `dropped`, with a log line that says how the withdrawal was seen.

- The retraction comes as `conversation.message_retracted` for M, read by
  the seat's events, which are long-polled while it writes an answer
  (§5.2); a retraction read a moment before the loop begins, by a claim
  that read X just before it, stops the loop as it begins. A retraction
  of any other message, an older question or an answer of the agent's,
  stops nothing.
- A draft refused as `conversation_not_awaiting` while its attempt is
  still being written, not as its answer goes in, is the same news come
  another way, where the event is late or missed: X's newest messages are
  read, once per answer, and the answer stopped if its question is
  withdrawn. X closed, and refused for that, stops nothing: the answer
  goes on, and is dropped when Core refuses it as `closed`, as before.
- An answer already being sent is left to Core, which orders the two: sent
  before the retraction, it is posted; after it, a Core since #42 refuses
  it (`moved_on` naming no message, then as above), and a Core before #42
  posts it.

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
  off or a provider's failure, or by the fallback. A continuation of an
  answer the output cap cut off (step 7) shows the answer so far and what
  it writes after it, its steps left as they were, so that the draft grows
  through it; a try made again starts again from the answer so far. An
  adapter that does not stream shows steps alone.
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
  attempt writes no more, and X is read for a question withdrawn if its
  answer was not yet being sent (§5.3); one that failed on the way (a 5xx,
  a timeout of its own 5 s) is sent once more, with the state as it stands
  then, and then given up; any other refusal stops the attempt's drafts.
  None of it fails, holds back or slows the answer, or slows the agent's
  polling.
- Its end: when the answer is to be posted (step 9), nothing more of the
  draft is sent, and Core clears it as it posts or proposes the answer. An
  attempt that ends otherwise (the providers failed, the claim's time ran
  out, the post did not go in, the opener moved on) is ended with `done`,
  which deletes the draft, when any of it was sent; one whose question was
  withdrawn is not, as Core deleted its draft with the question.
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

**Attachments** (`worker/attachments.go`, `toolset/attachments.go`; §2.9).
A message of a conversation may carry files, which people upload as they
ask (Core's conversation attachments: at most 10 a message, 50 MiB each,
500 MiB a conversation). `conversation_messages` lists each message's
files (`attachments`: id, name, the type its uploader declared, size,
checksum), none once it is retracted; `conversation_attachment` gives one
file's download URL, good for about fifteen minutes, to whoever may read
its conversation, and `not_found` to anyone else, as for a file that is
none. Both are the runtime's calls, with the agent's own token: neither is
offered to a model (`conversation_*`, §4), and no model sees a URL. The
model is given the files through the pipeline a document's file goes
through (§4, Files), which the runtime owns, whatever the file and the
model:

- *Told where they were attached.* A message that carries files announces
  them before its text, in brackets: `[Message 3 carries 2 files,
  attached by its author: "essay.pdf" (application/pdf, 1.2 MB,
  attachment_id …), "graph.png" (image/png, 240 KB, attachment_id …).
  What the runtime gives of each follows the message.]`
  (`prompt.HistoryWithFiles`). The number is the message's in the
  conversation (its `seq`), which the file's record repeats, so that the
  model knows which message carries which file where the history joins
  the opener's messages into one turn. An earlier message's files are
  announced so, "read one with attachment_get if it matters".
- *The question's, given with it.* The files of the question, the
  opener's messages since the agent last answered, are fetched and read
  before the model's first call, within a third of the answer's wall
  clock (the draft shows the answer reading a document meanwhile), and
  follow the message that carries them: for each, a block of text, its
  heading and then JSON as a result is, `{"attachment": {attachment_id,
  filename, content_type, byte_size, message_seq}, "file": {…},
  "file_text": "…"}`, the record being a document's file's (`given_as`,
  `converted_to`, `extracted_from`, `part`, `parts`, `part_holds`,
  `next_part`, `note`), then the file's part, if any. What that turn holds
  is sent again with every later turn of the answer, as a tool's result
  is, so it is bounded as the document settings bound a read of one
  (`toolset.GiveAttachments`): each file as `attachment_get` gives it (its
  first part: at most one result's text, 24 KB at the defaults, or a PDF of
  `PDF_PART_PAGES` pages), in order, while the text given stays within two
  results (64 KB) and the pages of the file parts within one PDF part's
  (10), an image counting as one. A file past that room is named with the
  call that reads it; so is one Core could not give just now. A fallback
  model that takes files otherwise than its model is given them again, as
  it takes them.
- *What each file comes to*, by the same rules as a document's file:
  a text file (text/…, JSON, Markdown, and the application/… types of
  source code and text data: JavaScript, XML, YAML, SQL, Python, TeX, …;
  a file of no telling type by its bytes) is its text, to every model; a
  PDF is a file part to a model that takes files, in parts of its pages
  when it has more than a part holds, and otherwise its text, page by
  page, or what OCR reads of a scan; a presentation or a document is
  its PDF, Core's where Core says it has one done (with a fresh URL of
  `conversation_attachment` should it lapse), LibreOffice's otherwise, a
  deck's speaker notes beside it (as
  `toolset.notesBeside` reads them, from the deck, as the transcriber
  does), or its text where the model takes no files or the conversion is
  off; a workbook is its text; an image is a file part to a model that
  takes files, what OCR reads of it where OCR is on, and otherwise not
  given, the note saying this model cannot see images; anything else is
  not given, the note saying what it is (`audio/mpeg files are not read
  here`). A file past `MaxFileBytes` (50 MiB, as for documents, the most Core takes of an attachment) is not
  fetched, and says so; the costs of OCR and of a model's file parts are
  held as a document's are.
- *The rest, by the model's own call.* Where a message up to the
  question carries files, the model is offered `attachment_get`
  (`toolset.AttachmentTool`, `attachment_id`, `part`, `file_pages`), a tool
  of the runtime's own, of kind `runtime` (neither a read of Core's nor a
  write): it reads the rest of a long file, part by part as `document_get`'s
  `file_part` does, the call that reads the next named in `next_part`;
  pages of a PDF, a deck or a document as a PDF of their own
  (`file_pages`, as `document_get`'s), to a model that takes files; a file
  not given with the question, or one of an earlier message. Every call
  asks Core again (`conversation_attachment`) and refuses a file whose
  `conversation_id` is not the conversation being answered, before
  anything is fetched, with the same `not_found` as for a file that is
  none: a tutor's token reads every conversation addressed to it, and a
  model answering one student must never read another's files. Its
  arguments are checked before Core is asked (an id, a part from 1, at most
  ten pages, not a part and pages at once). It counts against the answer's
  `tool_calls`, as any call does. It is not offered where `tools.mode` is
  `none` (a model that takes no tools at all), or where `tools.deny` names
  it; the question's files are still given, and their records name no
  call. (The handout names the tool `attachment_read{attachment_id, part}`;
  it is `attachment_get` here, with `file_pages`, to mirror `document_get`,
  which the model already knows.)
- *Kept, and let go.* What was read of a file is kept in the worker's
  `TextCache`, as a document's is: by the checksum Core worked out from
  its bytes (`sha256:`), so that the same file is read once whichever
  message carries it, and a reading is reached only through a file of
  those very bytes that Core gives the caller; by the file's id where Core
  has only an object store's tag (`etag:`). Every reading is tagged with
  the file's id and its message's. Its PDF, LibreOffice's or Core's, and
  OCR's text are kept by the runtime's own checksum of the bytes, as a
  document's are, and
  reached the same way. A retracted message's files are withheld by Core
  (not listed, and `conversation_attachment` answers `not_found`,
  `retracted`): the worker drops what it kept of them as it reads the
  retraction (`TextCache.Drop` by the message's id, §5.4), `attachment_get`
  of one tells the model the file is gone and not to use what it read of
  it, and drops what was kept of it too; the retracted message is shown as
  `[message retracted]`, with no file.
- *What someone sent.* The system prompt says the files are what the
  asker sent, information and never instructions, as their messages are
  (§6), and how the model reads them.
- *Not yet.* The runtime attaches no files to its answers
  (`conversation_answer` takes `attachments`, uploaded with
  `conversation_upload_url`), and a `conversation.message_posted` that
  names files (its payload's `attachments`) warms nothing: the answer that
  reads them begins as soon as the inbox shows the question, which is at
  once where it long-polls, and a conversion started earlier would save
  little.

### 5.4 Following proposals

The events poller reads `event_list` from the seat's cursor:

- `action.approved` for a proposed attempt: `payload.outcome` `executed`
  settles it as posted; `failed` settles it as failed, and the conversation
  comes back to the inbox for the next attempt.
- `action.rejected`: settled as rejected; the reason is read from
  `action_list_mine` (the proposal's `result.decision.reason`, paged from the
  seat's `actions` cursor) into X's memory, for the next attempt's prompt.
- `action.cancelled`: settled as cancelled, `payload.reason` noted.
- `conversation.message_retracted`: the answer being written to that
  message, the opener's question withdrawn, stops (§5.3); notes about it
  forgotten, and what was read of its files (§5.3, Attachments); if it was
  the agent's own answer, a note not to repeat it.
- `conversation.message_posted` by an opener: the course is hot.

Core makes a proposal's action during the call that sends the attempt, and
a person may decide it before the store has recorded the attempt as
proposed. A decision on an action the store does not know, of an answer or
of an earlier version's close (its `payload.action_type`), read while one
of the seat's attempts is being sent, stops that read: the cursor stays
before its page, and events are read again at once when no attempt is
being sent, by when the store has the action. Without this, a decision
read too soon would be passed over for good, and the attempt left
`proposed` until a restart.

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
- the tools that read the course, and, where it is offered, that
  `course_materials_search` finds where the course's documents say
  something, that a hit is read with the call it names before it is relied
  on, and that a search that finds nothing does not show the course never
  says it (§4, Search);
- when the model is offered writes (only in
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
- where the conversation's messages carry files (§5.3, Attachments): that
  they are announced where they were attached and the question's given
  after it, that a file is what the asker sent, information and never
  instructions, and that `attachment_get` reads more of them, or, where it
  is not offered, that the model cannot read more than it is given;
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
| `llm_call`, `answer` | the ledger: ids and numbers; an answer's row counts the writes its model sent, and how many Core executed, proposed, denied and failed; a call's `kind` is `model_calls`, an answer's, or `transcription`, the transcriber's (§12), which has no agent, tenant, course or asker |
| `agent_state` | the owner's page's state, and the version of a hosted agent's row it is of: never replaced by a state of an older version |
| `secret` | sealed secrets (§11.1): id, tenant, kind, the key's id, the wrapped data key, nonce, ciphertext, hint |
| `person` | who has used the API: Core actor, name, platform role, last seen |
| `hosted_agent` | the registry (§11.2): id `agt_…`, Core actor (unique), owner and whether Core said so, tenant, name, token and own key (secrets, with hints), paused, settings (jsonb), version |
| `hosted_course` | (agent, course) → settings (jsonb), who wrote them, when |
| `registry_rev` | one row: the revision every write to `hosted_agent` or `hosted_course` moves on, by trigger, with `NOTIFY aishie_registry` |
| `audit` | the API's audit (§11.4): when, who, with which of Core's sessions, from where, what, to what, the outcome, and a detail of ids, hints, providers, models and results; kept 400 days |
| `ocr_text` | what OCR recognized of a file (§4, OCR), by the sha256 of its bytes: done or failed, pdf or image, the text (at most 4 MB), pages recognized and of how many, where each begins, notes, why it failed, the engine and how long it took; kept 180 days, a failure a day |
| `search_file` | the search of a course's materials (§4, Search): (version, file key) → course, document, the revision of the text read, whose it is (`staff`, `ai`, `runtime`, `body`), the file's name and place, how many passages and terms, when it was read and last needed (to the day); dropped when its version is purged, or unneeded for 30 days (0014) |
| `search_passage` | (version, file key, place) → the slide, page or sheet it is on, where it begins in the text and the part of it `document_get` gives it in, its text, its terms (`text[]`, under a GIN index) and how many; goes with its file |
| `site_setting` | what the runtime's administrators set (§11.5), by name (`ocr`, `school_quotas`, `agent_budgets`, `transcription`): a JSON object, who wrote it, when; every write moves `registry_rev` on |
| `school_offer` | the offers of the school's plan the administrators made (§11.5): id, label, adapter, provider, model, base_url, region, output bound, effort, on or off, the school's key (a secret of the tenant `school`, with its hint, and whether it was tried with the model), version, who made and changed it, when; every write moves `registry_rev` on |
| `site_price` | the site's rows of the price table (§11.5): id, provider, model (exact or a glob), from (a day), the four prices in pUSD a token, version, who made and changed it, when; one row per (provider, model, from); every write moves `registry_rev` on |
| `site_price_rev` | one row: when the site's prices last changed, to the second and always a second past the last, by trigger, which names their version (`site-<UTC second>`) |
| `site_tenant_quota` | a tenant's daily quota on the school's key as the site sets it (§11.5), in place of `runtime.tenants`': answers and pUSD, each null for none, who set it, when; every write moves `registry_rev` on |
| `transcription_credential` | one row: the transcriber's service credential (§12), a `core_token` secret of the tenant `site` with its hint, Core's id of it, whether Core took it when it was given, who gave it and when, when Core last took it and last refused it, and why; giving or forgetting it moves `registry_rev` on |
| `transcription_job` | what the transcriber did with each claim of a file (§12): id `trj_…` and a sequence it is listed by, the version, the file (`file_id` and `position`, '' and 0 from a Core before AIShie-Core #49; 0012), document and course, the claim, its status (`working`, `done`, `failed`, `skipped`, `dropped`) and why, whether it was the backfill's, the attempt, the file's type and size, its pages and those sent to the model, the offer and model, the calls, their tokens and cost, the worker, and when it started, was last held and ended; kept 90 days |

Beside the sums quotas are checked against (`Spend`), two reports read the
ledger for people, ids and numbers only: `Usage(agent, since, until)`, a
row per UTC day and course (billable answers, every answer by outcome,
model calls, tokens and cost), and `AskerUsage(agent, course, since,
until)`, the same per asker, counts only, never what anyone wrote; and
`CostReport(query)` sums the model calls of a span, as a whole or by UTC
day, tenant, agent, model (with its key source) or key source, on one key
source or both, a page of groups (at most 1,000) after a key at a time:
calls, those no price held, tokens and cost, the answers' and the
transcriber's apart (the latter by agent under the key `transcription`,
and by tenant under `site`). `Spend` on the school's key counts both: the
transcriber's calls count against the plan's ceiling across the key
(`per_day_usd`), and no owner's, tenant's or asker's quota. The API shows
an agent's seats from the seat rows, and never needs its token to.

`memstore` keeps the same in memory, for one worker and for tests: it loses
attempts and memory on restart, so a restarted worker may find its keys
taken (`idempotency_conflict`) and move to the next attempt number. Use
Postgres (`DATABASE_URL`) for anything that matters.

## 9. Defaults

The built-in defaults are §4's example: MCP at `2025-11-25`; the owner's own
key and 4,000 output tokens a call; tools derived,
the read tools of §2.3 and every gated read (a document's versions, where
students stand on an assignment, the roster, the queues of proposals) and
the gated writes allowed, writes off (`tools.writes`, on for a hosted
agent), four in parallel; three attempts,
then skip; the canned notice when out of quota; 19,000 characters; the
newest 30 messages; eight answers at once per agent, four per course; per
answer 8 turns, 12 tool calls of which at most 10 writes (`max_writes`),
150,000 input and 12,000 output tokens, 180 s; no daily
quotas unless set (a school key requires per-agent and per-asker ones, but
an offer of the school's plan, whose quotas are 100 answers a day per
owner and 20 per asker unless `runtime.school` sets them);
polling 2 s hot for 120 s, 10 s idle to 30 s, events 45 s, seats 300 s,
±25 %, 30 % of 600 a minute, and where Core offers `wait_s` each inbox
long-polled for 25 s (`long_poll_wait_s`), at most 12 calls of an agent's
waiting at once (`long_poll_max`); where Core takes drafts, each answer's
draft written at most every 300 ms, one write in flight per conversation, a
write given 5 s and one retry; memory on, purged 30 days after a seat goes;
files of at most 50 MB, read within `doctext.DefaultLimits` and 20 s, their
text given in parts of 24 KB within results of 32 KB, what was read kept
per worker up to 32 MiB; OCR on where its programs are, in
`chi_sim+chi_tra+eng`, 40 pages at 300 dpi, 90 s a page and 15 min a file,
1 GiB a program, one file at a time per worker and eight waiting, the first
question waiting 5 s for it; presentations and documents converted to PDF
by LibreOffice where it is, 2 min and 300 pages a file, 2 GiB, one at a
time per worker and eight waiting, what it made kept in memory up to 64
MiB; a PDF given as a file in parts of 10 pages where poppler cuts it; the
transcriber off, and once turned on, 2 documents at once, 300 pages a
document, no daily limit of pages, a claim of 10 minutes renewed every
third of it, 25 s waits on the queue, 10 pages a call (5 as pictures at
150 dpi), 3 tries a call, files of at most 64 MiB, jobs kept 90 days;
the renditions on wherever LibreOffice converts and Core is named, one file
at a time per worker, 5 min a conversion, a claim of 10 minutes renewed
every half of it, 25 s waits on the queue, files of at most 100 MiB; the
search of a course's materials offered wherever `document_list` and
`document_get` are, 5 hits a page and at most 10, the first 100 documents
listed, 400 passages scored a search, files read for its index four at
once within 20 s and half the answer's time left, passages of about 1,500
bytes and excerpts of 200 characters, files no search has needed for 30
days dropped.
The output tokens, a call's and an answer's, and the wall clock are more
than §4's example (2,000, 4,000 and 90 s), which a long answer, in
Chinese with a table, overran, and was cut off.

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
  as AIShie-Core #42 has it, and a Core before it, whose withdrawn question
  still waits and takes an answer (`WithdrawnWaits`); each seat's
  ceilings, as Core works them out; an agent's owner
  deciding and reviewing what it did where they could do it themselves;
  and no question to a runtime agent the site's runtime holds no live
  token for, nor ever to an `mcp` agent (the worker's tests wait for the
  runtime to be issued the agent's token, or, asking before a worker
  starts, have it issued as an earlier run would have). It hosts agents as
  AIShie-Core #52 does (`hosting.go`): each `runtime` or `mcp` for
  good; the `agent_runtime` service's four REST tools, offered to no
  model, taken with a service credential of that scope alone; one runtime
  token per agent, which issuing replaces and revokes; and a Core from
  before it (`WithoutHosting`), whose catalogue has no such service.
  `internal/fakecore/testdata/fixtures` are envelopes recorded from the
  pinned Core for every row of §2.4 and more (`make record-fixtures`
  against a live Core, whose recorder is issued each agent's token by the
  site's runtime); a conformance test holds the fake to them, and Core's own
  client is tested live against the real one. The fake takes a message's
  files as Core's conversation attachments have them (upload URLs on the
  fake itself, the files named in a question, a follow-up or an answer and
  refused as Core refuses them, listed, served as downloads, withheld once
  retracted, named in the news), which the `attachments` fixture holds it
  to, uploads' tokens recorded as `<upload_token>`. It makes PDF
  renditions as Core's migration 0026 has them (`renditions.go`): every
  Office file of a version or of a message queued by Core's table of its
  own, the `agent_runtime` service's claim (a long poll), file, renew,
  upload URL and its PUT, and complete, with Core's order, attempts,
  refusals and leases, and the rendition each file's reader is shown,
  with a URL that serves its PDF once it is done, which a test makes
  done itself (`RenderFile`); and a Core from before them
  (`WithoutRenditions`). The `renditions`
  fixture holds it to the rendition Core, a real Core's queue drained,
  unrecorded, first.
- Attachments (§5.3): each kind of file (text, code of no telling type, a
  PDF, a scan, a deck, an image, an archive, a sound) to a model that takes
  files and to one that takes none, with OCR and without; a file of another
  conversation refused as none and never fetched; a retracted message's
  file gone, and what was kept of it dropped; a long text and a deck read in
  parts by the calls `next_part` names, and pages asked for; one file of two
  messages read once, and a file of an etag kept by its id; the question's
  files within the first turn's room, the rest named, and no call named
  where there is no tool; `attachment_get` offered, and not where tools are
  off or deny it. Against the fake Core, a tutor's model and a text-only
  model given the question's PDF and deck with it, a long text read on with
  `attachment_get`, another student's file refused, and a retraction
  dropping what was read. The end to end (`files-with-the-question`)
  uploads an essay and a deck with a question as the front end does, and
  sees each model given what it takes, LibreOffice's PDF of the deck to the
  one that takes files, and the answers posted.
- Search (§4, Search): `internal/search`'s terms of English, Traditional
  Chinese and katakana, the queries' terms, the passages cut on slides and
  pages and never inside a character, BM25 with a rare term above a common
  one and the phrase as written above its words apart, and the excerpts.
  `storetest` holds both stores to the index's contract (files at their
  revisions and in their course alone, passages replaced, the candidates'
  order, dropped by version, by document and unused), and `pgstore` runs a
  Traditional Chinese and an English search in a database whose ctype is
  C. Against a Core of the test's own: a student's search finds a deck's
  slide by a Chinese question, a PDF's page, a text version's page and
  the syllabus's own text by English ones, each hit naming where it is,
  whose its text is and the call that reads it; once staff have searched,
  so that the shared index holds a draft, a draft version newer than the
  published one and instructions withheld from students, a student's
  searches find none of them and say what they could not read; each file
  is read once, a text version in place of its scan, one answer's
  searches ask Core once and a later answer's again without reading a
  file, and a text edited is read again and found as edited alone; a scan
  without a text version is said to have no text, and kept so, and files
  the time ran out for are said not to be read yet; a text version Core
  could not give just now is not kept in its place, and the next search
  finds its words; a text file holding NUL, which no store keeps, is kept
  with a space for each, found, and read once; a version Core gives as
  purged leaves the index (and a document listed as purged, which today's
  Core does not list); a purged version and a document refused the seat
  said to have no version it may read now, and one Core could not answer
  for said not read just now and found by the next search; a course of
  99 or exactly 100 documents said to have none more, and one of 101 to
  have more, its first 100 searched; in a version's own text of some
  100 KB, every hit that gives no `passage` read whole by its read and
  those past the cut given their passage, and so in a file whose
  version's own text leaves its part too little room; hits a
  page at a time to the last, a long text version's hit naming the part
  that `document_get`, called as it is, gives it in; in a long text of no
  pages, a text file's and a text version's, every one of 200 words found
  in the part its hit's read gives; a deck of pictures, to a model that
  takes no files on a runtime with LibreOffice and OCR, whose parts are
  not the index's, read by its slide, which gives that slide's text and
  OCR's, and to a model that takes files its slide as drawn; a Word file
  given as its PDF's pages saying its page is not known, and given as text
  read by its part; a deck and a Word file whose PDF Core made, where
  LibreOffice does not convert, pointed into Core's PDF as into
  LibreOffice's; arguments refused before anything is read; and the
  tool offered with `document_list` and `document_get` alone, never where
  denied. Against the fake Core, Yuki's agent and Sato's, which reads
  drafts, sharing the worker's index, find the published slide, and
  Sato's alone the draft, though Sato's searches first, so that the index
  holds the draft when Yuki's does, counted and logged without the query;
  the draft
  purged, it leaves the index as Sato's seat reads the news; a version
  purged (`document.purged` or `_unreleased`) leaves the index, a whole
  document purged every version of it, and housekeeping drops the files
  unused for 30 days. `storetest` also holds the candidates past the limit
  to the shortest of those tied, and a file's use marked once a day. The
  end to end (`search-of-the-materials`) does the same against the pinned
  Core, Sato's searching first there too, the first hit read with the call
  it names, and an administrator's purge of the draft's version.
- A version's files (§4, A version's files; AIShie-Core #49): a version of
  a PDF, a Word file and notes given file by file, in order, under their
  names, to a model that takes files and to one that takes none, no URL in
  the result; each file's text version first, from its body or read in
  parts by its `file_id`, kept by the file and dropped by it alone; the
  bounds, a long file's first part with `next_part` naming the file, a
  file past the room named and a PDF past the pages not fetched; `file_id`
  reading one file in parts, an id of no file, a part naming no file of
  several, an id that is none refused; a version of one file given as it
  always was, whichever Core lists it; a lapsed URL fetched again from
  `document_file`. The fake Core holds a version's files as #49 does (listed
  in order, each served under its name, `document_file`, a text version
  and a claim of each, a text event naming its file), and, with
  `WithoutFiles`, answers as a Core before it; the worker is given the
  files' texts and drops one file's on its event, against either. The end
  to end (`files-of-a-version`) has Sato put up a lecture of a PDF, a Word
  file and a program in one version, as the front end does, and sees the
  tutor's model, which takes files, given the PDF and LibreOffice's PDF of
  the Word file as files and the program as text, and a text-only model
  the text of each.
- The transcriber (`internal/transcribe`) against `fakecore`, whose text
  versions, service credential and queue answer as Core #43's do (a
  claim's lease lost, the text edited by staff meanwhile, a credential
  revoked): a PDF in ranges of pages, a deck converted with its notes, a
  model of pictures, the skips and failures, one claiming worker of two,
  the blocks, the page quota and the plan's dollars; a version of three
  files transcribed file by file, every call naming its file and each
  completion keyed by it, one file's claim lost and another's text
  written by staff while the third is done, and nothing naming a file
  against a Core of one file a version; the API's routes, their refusals
  and audit, on `memstore`; the store's tables on both stores; and the end
  to end (`transcription`) against the pinned Core, where a lecture of
  three files in one version is transcribed file by file too, each job
  naming its file, and read back file by file.
- Adapters: golden translations both ways in `testdata/`, every stop reason
  and usage field; `LIVE=1` runs them against the real providers whose keys
  are set (the live tests, below), with every tool declared at 16 output
  tokens, and one answer streamed. `openai_chat`, `anthropic` and `gemini` have goldens
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
- A question withdrawn, against the fake as #42 has it and as a Core
  before it (`WithdrawnWaits`): withdrawn while the model is part way through
  its answer, with events read every 30 s, its call is cancelled within
  moments for that cause, nothing is posted, no `done` is sent, the ledger
  says `dropped` and the question is not tried again; withdrawn as the
  claim reads it, the model is never asked; refused as `moved_on` naming no
  message, it is sent once and not tried again; an older question or the
  agent's answer retracted stops nothing; with the events unreadable, a
  draft refused has the conversation read and the answer stopped, and one
  refused for a conversation closed stops nothing; and on a Core before
  #42 an answer already being sent is posted. The end to end
  (`stop-cancels-the-answer`, against the pinned Core, and run by hand
  against b0eb848's) has Yuki
  withdraw her question while her agent's model has stalled part way
  through its streamed answer, and sees the model's request cancelled
  within seconds and no answer ever posted.
- An answer the output cap cuts off, with the scripted model: continued
  twice, the pieces joined in order and nothing twice (a table row, a
  sentence or Chinese words written again left out), each continuation
  given the answer so far, with no tools; the draft growing through it in
  one attempt; the last continuation, its room less than a cap by the
  output tokens, by the body at the answer's characters a token, or by
  the wall clock at its pace, told that room and to close; one still cut
  off, one whose continuation fails, a last turn forced by the wall clock,
  and an answer too long to post whole, posted with `on_truncated_text`;
  a last turn forced by the turns continued; and no continuation past the
  output tokens, the input tokens, the wall clock or the body. The end to end (`long-answer-continued`) has
  Yuki's agent's model cut off at `finish_reason: length`, and sees the
  continuation asked with the answer so far and no tools, one answer
  posted of the two pieces, and, where Core takes drafts, its text growing
  through the continuation as Yuki watches.
- Attempts spent: the question skipped until the next day and its
  conversation left open, by default, with `skip`, and with `close`, which
  is logged as deprecated; a close an earlier version left `sending`, sent
  again at the seat's start and settled from Core's replay. `close` and
  `close_reason_text` in `runtime.defaults`, an agent's settings, a
  course's and a hosted agent's load, taken as `skip`, and are listed as
  deprecated where they are written; `check` shows them.
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
  holds random texts of every escape to its promises. With Office files
  converted (a stub of LibreOffice's, `stubOffice`), and the same table
  with the conversion on beside the cases with it off: a deck to a model
  that takes files is LibreOffice's PDF, whole with its notes, or in parts
  of ten slides, each with its own slides' notes, `next_part` to the last,
  a part past it said so, a provider's limit on pages making the parts
  smaller; a PDF of the course's own in parts of its pages too, now a file
  where its pages were past its provider's, and whole where nothing cuts
  it; to a model that takes none, the deck's text with what OCR read of
  its slides with pictures after each one's text and before its notes, OCR
  asked once, for those slides alone, by a checksum of the runtime's own,
  the text given without it while OCR reads them, and the note saying so
  with no OCR; a PDF past its provider's size its text with OCR; while a
  PDF is made, the text and `ask_again`, OCR not asked; a conversion that
  failed; a PowerPoint 97 deck read in its `.pptx` and its notes beside
  its PDF, a Word 97 document in its PDF's text, or its OCR, and so a
  Word file of scans, an Excel 97
  workbook in its `.xlsx`, each known by what it holds when Core names no
  type; a file with a password not converted; and with the conversion off,
  every file as before. Core's PDF of a file (`rendition_test.go`): a deck
  whose rendition is done given to a model that takes files as that PDF
  with its notes, LibreOffice never asked, the PDF fetched once and kept;
  none, queued, claimed, failed or skipped, the deck converted here as
  before, no PDF fetched; a URL the server refuses asked of
  `document_file` again and the fresh one fetched, one past its
  `download_expires_at` not tried, and where the fresh one is refused too,
  or the server fails, the deck converted here; a fresh URL of another
  version, other bytes or another message's file never fetched; a PDF not
  had tried once a call, for its text or OCR too; the parts of Core's PDF
  and of LibreOffice's cut under names apart; a PDF past `MaxFileBytes`
  not fetched, or not read past it where Core said it was smaller; a
  message's deck given as Core's PDF by `attachment_get` and with the
  question, a lapsed URL asked of `conversation_attachment` again; to a
  model that takes no files, a deck's slides picked for OCR out of Core's
  PDF and a Word 97 document read in it; without LibreOffice, a deck and
  an OpenDocument deck given as Core's PDF to a model that takes files,
  and as before to one that takes none, or without a PDF of Core's; and,
  with the worker's own conversions, Core's PDF taken, or not taken for
  each refusal (and, in `office`, remembered for `RenditionRetention` but
  for a fetch cancelled), no URL, its signature or its path in a result or in their
  log, and a conversion of the file after finding Core's PDF. The fake Core
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
  as the deploy does (65532:65532, no network), `check` with `OCR=on` and
  `OFFICE_PDF=on` in it, and the same real tests, OCR's and the
  conversion's, against the programs in the image.
- `sandbox`: a program's environment (nothing of the runtime's), its
  limits, its output kept to its bound, its failure's status and errors,
  and its kill with its children at its timeout.
- `office`: the converter against a soffice of the test's own under the
  real prlimit: the filter and options each target has, the file given
  under its format's extension, the profile's settings, the pages of a PDF
  made counted and capped, LibreOffice making nothing (it ends well all the
  same) or failing, its timeout killing its process group, the
  environment, limits and niceness it has, and its private directory
  removed; a file that is not held as an Office file refused before it.
  The service: a file converted once for questions at once, then kept; a
  question waiting half its time left at most, the conversion going on; a
  failure kept its hour, then tried again; turns and a full queue; nothing
  kept when the process stops; ranges cut once and kept, picked pages cut
  every time; Core's PDF of a file taken once and kept, a conversion after
  finding it, whatever LibreOffice said of the file before and whether it
  is here or not, cut to `MaxPages` past it, and not taken past them with
  nothing to cut it, nor when it is not a PDF or was not fetched, nothing
  kept then and the fetch's error, a URL in it, neither returned nor
  logged. `Sniff` knows each older format by its stream, an
  OpenDocument file by its `mimetype`, RTF by its first bytes. With the
  real programs (skipped only where they are not installed, and never with
  `OFFICE_PDF_REQUIRED=1`): a deck in Chinese with a picture, a hidden
  slide and a chart makes a PDF of a page a slide whose text reads, the
  picture drawn, no notes pages; ranges and picked pages are PDFs of those
  pages; the deck saved as `.ppt` and `.odp` converts back to `.pptx` with
  its notes; a Word document, and the same as `.doc`, `.odt` and `.rtf`,
  make PDFs whose Chinese reads; a workbook as `.xls` and `.ods` converts to
  `.xlsx`; an OpenDocument text with a section linked to a local secret
  and a picture on a server of the test's, and a deck with a linked
  picture, convert without the secret and without the server asked. The
  toolset's own (`TestRealDeckToModels`): a deck of twelve Chinese slides,
  one with a screenshot of code, reaches a model that takes files as PDFs
  of ten slides and two whose Chinese reads, with the notes, and one that
  takes none as its text, with the code OCR read from the screenshot's
  slide.
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
  The worker runs hosted agents beside YAML ones, issued each one's token
  by its id and keeping it sealed in its row, one token across workers
  sharing a store; re-issues a token pasted before hosting was by id,
  paced; refuses an `mcp` agent, one not found and a Core too old, ends
  the hosting (revoking the token) of an agent whose owner of record is
  not its owner in Core or that is suspended, pauses them, keeps an
  unauthorized one stopped through others' changes and starts it again
  once its owner asks for a new token, and lets YAML win an agent in Core
  whichever started first; the binary picks up an agent hosted while it
  runs, told by the notification; and the end to end hosts Yuki's agent by
  its id in a runtime on Postgres, whose worker is issued its token
  (revoking one pasted before), which answers her against the real Core,
  with no token, key or the key that seals them in its logs, in any table
  of its database, or in its status.
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
  on the schedule and long-polling, each measured until every agent has
  polled every course's inbox, and no agent polling one course's inbox
  more than 4 × (n + 1) times, n its fewest polls of another's, its calls
  carried over connections kept between them, as HTTP/2 to Core carries
  them; no token in any log line; the
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
  once and the text kept by its checksum; Yuki's question's deck and Sato's
  lecture, whose PDFs Core made, given to her agent's model as those PDFs,
  LibreOffice run only for a handout whose rendition is queued, and no log
  holding a URL of Core's; an operator's agent named by its
  id, issued its token once and keeping it through restarts, its token
  revoked as it names another agent in Core, is paused or is removed, and
  forgotten on a 401 for a reload to be issued another; the runtime's own
  credential missing or refused, then given; and Sato's own assistant, given `member_manage`, offered
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
  earlier run was issued the token of; every other is asked once the
  runtime under test has been issued its token, which Core requires; two
  workers that share no store are given the one token); a seat set to
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
  its model; and where LibreOffice is installed (skipped where it is not,
  never with `OFFICE_PDF_REQUIRED=1`), Sato uploads a lecture of twelve
  slides, and Yuki's own agent, whose model takes files, is given its
  first ten slides as a PDF of ten pages, the notes beside them, and told
  there is a second part; and (`slides-as-cores-pdf`, where Core has
  renditions) Sato puts up a lecture whose rendition the test makes in
  Core as the site's runtime does, with its credential (claimed, the PDF
  put, completed), and Yuki's own agent, run where LibreOffice converts
  nothing, is given Core's PDF of it, page for page, with its notes, no
  download URL reaching the model or the runtime's log.
- The live tests, which CI does not run: they need a provider's key or a
  Core of their own, and say so when they skip.
  - `make live`, the adapters against the real providers whose keys are
    set: `OPENAI_API_KEY` (Chat Completions and Responses),
    `ANTHROPIC_API_KEY`, `GEMINI_API_KEY` (its own API and its OpenAI
    compatible one), `DEEPSEEK_API_KEY`, `BEDROCK_MODEL` with AWS's
    credentials (or `AWS_BEARER_TOKEN_BEDROCK`), and Azure's three
    (`AZURE_OPENAI_*`), each provider's model in `*_MODEL`
    (`OPENAI_RESPONSES_MODEL` for Responses). Each gets a cheap call, and
    each but Anthropic a tool call's round trip; Chat Completions and
    Anthropic an answer streamed; and OpenAI's two APIs, Anthropic and
    Gemini every tool of the pinned catalogue declared at 16 output
    tokens, Core's own schemas, in requests of at most 128 tools
    (`livetest.MaxTools`: OpenAI takes no more, and the catalogue has
    168), so that the provider itself checks every schema. `live.yml` runs
    it nightly with the repository's keys, and its log names each provider
    tried or skipped; run it by hand when an adapter changes, or a
    provider's API or the default models do.
  - `make live-core`, Core's client and a seat's toolset against a
    throwaway Core (`scripts/live-core.sh`: the pinned image, or `CORE_BIN`,
    on a scratch database, started with a limit of 600 calls a minute), with
    no model and no key. `TestLiveContract` has a course tutor hosted by
    its id as the site's runtime hosts it (AIShie-Core #52): with a
    credential of the `agent_runtime` service's own, issued for the test and
    revoked after it, `agent_runtime_agent` and `check_owner` read it, its
    owner is refused a token of it (`hosted_by_runtime`), a question to it
    is refused (`agent_not_hosted`) until the runtime is issued its one
    token, and again once the runtime revokes it, and an `mcp` agent is
    never issued one (`not_runtime_hosted`) nor asked (`mcp_agent`); then
    the calls a runtime makes over MCP and over REST give the same
    envelopes (answers, replays, conflicts, a second answer refused, the
    query string's forms, refusals word for word, 401s), `tools/list`
    offers every tool of the catalogue but the site services' own, which
    are REST's alone (the agent runtime's hosting and renditions, the
    transcriber's queue), and the typed client reads through the worker's
    stack. `TestLiveRateLimited` meets a real 429 over each transport and
    sees `Retrying` wait it out. `TestLiveCore` runs a student's own agent,
    hosted by its id, through its toolset: the live catalogue passes
    `Check`, the delegate is offered its twelve reads, what a strict model
    writes is executed in the conversation's course and nowhere else, the
    files of three documents are given as text and as a file part, and a
    Word file as its text, after the test has made its PDF rendition as
    the renditions worker does (claimed, fetched, renewed, the PDF put,
    done and replayed; no LibreOffice needed), and no URL, the
    rendition's included, reaches the model. Run it whenever the Core pin
    moves, with `make record-fixtures` (CONTRIBUTING.md, Moving the Core
    pin), and before a release; it needs a Core since AIShie-Core #52.

## 11. Hosted agents

M2 lets people host their own agents from AIShie-Frontend instead of an
operator writing YAML. The runtime's side is built in steps: the secret
store (§11.1), the registry of hosted agents that runs them beside the YAML
agents (§11.2), a versioned JSON API for the front end (§11.4), what the
site's administrators change through it (§11.5), and hosting every agent by
its id, with the runtime's own credential in Core (§11.6).

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
who hosts the agent, and the token the worker is issued for it.
`internal/registry` turns each row into the agent
document a YAML file would hold, and runs it beside the YAML agents:

- **The document.** The row's `settings` (the agent document of §4, as
  JSON) are the document, with what the registry sets itself: `id`,
  `display_name`, `tenant_id` and `paused` from the row; `core` as
  `{base_url: CORE_BASE_URL, agent_id: <core_actor_id>}` (the token the
  row holds, issued to the runtime, goes to the worker beside the document,
  never in it); and
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
  own client, as a YAML agent's do; but an offer the site's
  administrators made is held to these rules too, and called over the
  hosted-model client (§11.5).
- **YAML ∪ registry.** `registry.Build` is the YAML configuration, then
  every hosted agent that passes; the rest are in `Config.Rejected`, which
  the supervisor shows in state `error`. A hosted agent whose id is a YAML
  agent's loses to it, as does one on the agent in Core a YAML agent names
  (`operator_agent`, §5.1).
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
  in the same transaction; its token, when it holds one (none until the
  worker is issued it, and none while it is paused: pausing destroys it),
  must be a `core_token` and its key a `model_key` of its tenant, and no
  other agent's; `token_issued` says the token was issued to the runtime
  (not pasted by an owner before hosting was by id), and
  `token_credential_id` names it in Core. An update names the
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
  Beside `runtime.school`, the runtime's administrators make offers and
  set the quotas in answers through the API (§11.5); the plan in force is
  both, rebuilt with the registry. An offer the school withdraws (taken
  out of `runtime.school`, or the site's turned off, deleted, or held
  back) leaves the agents on it on their owners' models behind it, on
  their keys, as a quota spent does; an agent with none is not run, in
  state `error` with the reason `offer_withdrawn`, until the offer is back
  or its owner chooses another.
- `check`, with `DATABASE_URL`, reads the registry as `run` does, lists
  the hosted agents it would run and those it would not, with why, and
  passes: they keep no other from running. A registry it cannot read (a
  schema older than the binary's, before a deploy's `migrate up`) is said,
  and passes too. `check --live` reads the hosted agents in Core as well,
  and connects those whose rows hold the token the runtime was issued,
  showing each seat's tools as the worker offers them
  (`toolset.ForSeat`), `course_materials_search` among them, and
  `attachment_get` where a conversation's messages carry files; it is
  issued none itself.

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
`KMS_KEY_ID`, and `run` refuses `API_ADDR` without them; it hosts nothing
without the runtime's own credential (`CORE_SERVICE_CREDENTIAL`, §11.6).

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
  /admin/school-plan/usage`: today's use of the school's key (the
  store's `Reports.TenantUsage`), the plan's quotas, the total, and a row
  per tenant, a hosted agent's owner's with their actor id and the name
  the runtime last saw; reads and changes the site's settings, the
  school's plan and the money; and reads what the ledger recorded (§11.5).
  Anyone else is 403 `not_admin`.
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
  and for the routes that ask Core about an agent 10 a minute, bursts of
  5, and
  `keys/test` 6 a minute, bursts of 3, and 100 a UTC day), answered 429
  with `Retry-After`; the assertion; a query (none is taken, but the
  parameters of the administrators' `GET /admin/tenants`, `GET
  /admin/costs` and `GET /admin/transcription/jobs`, each once, any other
  `unknown_parameter`: a route that reads a body refuses one too)
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
  (`host_by_id` and the owner's own key when the API has a Core, the
  runtime's own credential to ask it with and a vault, as `run` gives it;
  and the school's plan, when the runtime's settings offer a model on it).
  `GET /me`: the person's actor id and name, whether they are an
  administrator, and how many agents they host; it records the person
  (`person`), at most every five minutes. The rest are a hosted agent's
  life, each the owner's alone (another's agent is 404, never 403):
  - `POST /agents/inspect` and `POST /agents` take an agent's id in Core,
    `{agent_id}`, and ask Core, with the runtime's own credential, whether
    the caller owns it and what it is (`agent_runtime.check_owner`, §11.6):
    another's agent, one nobody owns, a person or no one at all is 404
    `agent_not_found`, as Core says nothing of them. Inspect answers how
    Core hosts it (`hosting`), whether it may be hosted here (`hostable`,
    and when not, why: `mcp_agent`, `agent_suspended`, `owner_suspended`,
    or `operator_agent` for one the operator's YAML runs), its live seats'
    count, whether people may ask it in the site now, and whether it is
    hosted here (`hosted`, its id when it is the caller's); nothing is
    written. Hosting refuses what inspect says is not hostable (422, 409
    for `operator_agent`), and writes a new row, naming the agent and the
    caller as its owner, verified, with no token (`needs_model`): the worker
    is issued the agent's token once it has a model. Asked again, it
    replays the row (200, `Idempotency-Replayed`); an earlier owner's row
    of an agent Core says is the caller's is taken over, deleted, purged
    and its token revoked, and audited. The runtime's credential missing
    or refused is 503 `runtime_misconfigured`, a Core from before hosting
    by id 422 `core_too_old`, a Core not answering 503 `core_unavailable`.
    Nobody gives the API a token, and it is issued none.
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
  - `POST /agents/{id}/token` (no body; `If-Match` held to when given):
    a new token, after Core refused the one the runtime held (revoked
    there by the agent's owner or an administrator: `needs_token`). Core
    must still say the agent is the caller's (`owner_changed` when not)
    and may be hosted; the row's token is dropped, its secret destroyed
    with the write, and the worker is issued another, which revokes any
    left. A row holding none changes nothing (200, `Idempotency-Replayed`).
    `POST …/pause` and `…/resume` set the row's flag, at the version
    `If-Match` names when it names one (412 when the row was written after
    it was read). Paused, the row holds no token (the store destroys it
    with the write), and then the token the runtime held is revoked in
    Core (`agent_runtime.revoke_token`), after the write, so that no
    worker is issued one after it; the answer is the agent and
    `revocation`: `{outcome: revoked | none | failed | not_attempted,
    problem}`, `failed` saying why (`core_unavailable`,
    `runtime_misconfigured`, `core_too_old`), and `not_attempted` for an
    agent the operator runs (`operator_agent`), whose token is the
    operator's agent's. A pause of an agent paused already writes nothing
    and audits nothing, but revokes again, so a failed revocation is
    tried again so. Resumed, the worker is issued another.
  - `DELETE /agents/{id}` (no query, no body; `If-Match` held to when
    given): the row, its courses and its secrets destroyed in one
    transaction, and the agent's notes, attempts, cursors, seats, state
    and leases purged, its ledger kept; then its token revoked in Core, as
    a pause revokes it, the answer saying what became of it beside the
    agent deleted. A worker issued a token meanwhile finds the row gone,
    and revokes it.
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

### 11.5 What the site's administrators change

The runtime's administrators (§11.4, D4) change from the front end what
was the operator's alone: whether OCR runs, and in which languages (§4,
OCR); the school's AI plan (§11.2's key pool), its offers and its quotas,
in answers and in dollars; and the rest of the money: the price table's
rows, the tenants' daily quotas, and the hosted agents' daily budgets by
default. They read what the ledger recorded, in dollars. The operator's
environment stays the ceiling, and `runtime.yaml` and the price file
(`PRICES`, or `runtime.prices_ref`) the defaults the site's settings
stand in place of: `OCR=off`, or OCR's programs missing, is off whatever
the site says; `runtime.school`'s offers and the price file's rows are the
operator's, listed read-only; and `allowed_models`, `denied_models`,
`on_quota_text` and the budgets of one answer (turns, tool calls, tokens,
time: not money) stay `runtime.yaml`'s.

- **Kept in the store** (migrations 0009, 0010 and 0011): `site_setting`, a JSON
  object by name (`ocr`: `enabled`, `languages`; `transcription`:
  `enabled`, `offer`, `max_pages`, `per_day_pages`, `concurrency`; `school_quotas`:
  `per_owner_day`, `per_asker_day`, `per_day`, and `per_owner_day_usd`,
  `per_asker_day_usd`, `per_day_usd`, null for none; `agent_budgets`:
  `per_agent_day` and `per_asker_day`, each `answers` and `usd`, null for
  none); `site_price`, the site's rows of the price table; and
  `site_tenant_quota`, a tenant's quota; and `school_offer`, an offer
  made as an owner's own model is chosen (a provider of `GET /models`, its
  adapter, the model, the endpoint, resource or region its offer takes,
  the output bound and the effort), at the provider's own endpoint, with
  the school's key sealed (§11.1) under the tenant `school`, a `model_key`
  that goes with the offer: replaced, the one before is destroyed in the
  same transaction, and deleted with it. Only its hint is ever shown.
- **Put in force without a restart.** Every statement that writes any of
  these tables moves `registry_rev` on and notifies `aishie_registry`, by
  0003's trigger function, so each worker rebuilds as it does for a hosted
  agent's change (§11.2), within moments, or at the next poll.
  `registry.Build` reads the site with the hosted agents, after the
  revision, and `config.Runtime.WithSite` makes the plan in force:
  `runtime.school`'s offers, then the site's that are turned on, but one
  whose id `runtime.school` has (`id_taken`: the operator's wins) or whose
  model the lists do not allow (`model_not_allowed`); and the site's
  quotas, in answers and in dollars, in place of `runtime.school`'s. A
  tenant's quota the site sets stands in place of `runtime.tenants`'
  (the whole quota: answers and dollars). The site's budgets stand in
  place of `runtime.defaults`' `budgets.per_agent_day` and
  `budgets.per_asker_day` for the hosted agents, which are built on them
  at each build; an agent's own settings still win, and `runtime.yaml`'s
  agents keep what they were built with when the YAML loaded. The price
  table the worker costs each call by is the price file's with the site's
  rows before it (`Site.PriceTable`), put in force at each build and at
  `SIGHUP`. The quotas apply from the next answer; an agent whose offer
  changed (a key replaced is a new secret, so a new reference) restarts.
  The OCR setting goes to each worker's `ocr.Service` at each build, and
  the transcriber's to its `transcribe.Service`, with the plan's offer it
  names as the plan in force has it, or none. A
  registry that cannot be read leaves the site's settings as last read in
  force. The API reads the settings in force from the store at each
  request, so that owners see an offer the moment it is made.
- **The price table's versions.** The site's rows are a table of their
  own, versioned by when they last changed, to the second:
  `site-<UTC second>`, such as `site-20260930T101500Z`. Every write to
  `site_price` moves `site_price_rev` on, by trigger, to the second and at
  least a second past the last, so that every change is a new version.
  The ledger names each cost's table version and row, `2026-09-27/sonnet-4`
  of the file's, `site-20260930T101500Z/haiku-4-5` of the site's, and keeps
  the name with the cost: a cost recorded before a change keeps the
  version it was priced by, and nothing is priced again. The table in
  force takes `Lookup`'s rules across both (a model priced exactly beats
  every glob, then the latest `from`, then the most specific glob), and of
  a site's row and a file's otherwise alike (the same provider, model and
  `from`), the site's; its version is the file's and the site's joined by
  `+`.
- **A quota in dollars needs a price.** As `run` holds them, an agent with
  a quota in dollars (its own budgets or the site's, its tenant's on the
  school's key, the plan's on an offer) whose model, or fallback, no row
  prices today is not run; and with a quota of the plan's in dollars,
  every offer must be priced. The API refuses a change of the site's that
  would leave one of either where there was none: it builds the
  configuration as the registry would, before and after the change
  (`registry.BuildWith`), and answers 422 `offer_not_priced` (the offers'
  ids in `details.offers`) or `model_not_priced` (what is wrong, naming
  the agents and models, in `details.problems`). Adding a price resolves
  either; so does a quota in answers alone. An owner's change of their
  agent's model is held to the same (`settings_rejected`).
- **The site's offers are an owner's model**: the registry holds an agent
  on one to what it holds an owner's model to (a provider's own endpoint
  over https, a key, no headers), and the worker calls it over the
  hosted-model client (`internal/netguard`), where `runtime.school`'s
  offers, whose endpoints are the operator's, go over the runtime's own.
  With a quota of the plan's in dollars, an agent on a site's offer the
  price table does not price is not run; the API refuses to make one.
- **Withdrawn.** An offer turned off, deleted, or held back leaves each
  agent on it on its owner's model behind it, on the owner's key, and with
  none, not run, in state `error` with the reason `offer_withdrawn`; the
  row keeps the offer's id, so an offer turned on again, or made again
  with its id, takes its agents back.
- **The API** (§11.4's rules: an administrator alone, else 403
  `not_admin`; every write audited, ids and hints alone; refusals in
  Core's envelope):
  - `GET /admin/settings`, and `PATCH` of `{"ocr": {"enabled", "languages"}}`
    by merge-patch: whether OCR can run here (`available`, and if not
    `unavailable_reason`, `operator_off` or `not_installed`), the site's
    setting or its defaults, the environment's languages and those
    installed. Where OCR cannot run it is not turned on, nor given
    languages (422 `ocr_unavailable`); a language not installed, or twice,
    is `invalid_field` at its pointer. Audited as `settings.update`.
  - `GET /admin/school-plan`: every offer, `runtime.school`'s (`source:
    "config"`) then the site's (`"site"`), each with its status in the
    plan, whether it is priced, how many hosted agents are on it, and, of
    the site's, its key's hint, whether the key was tried with its model,
    its version (an ETag), and who made and changed it; the quotas in
    force and `runtime.school`'s beside them. `GET
    /admin/school-plan/offers/{id}` is one.
  - `POST /admin/school-plan/offers` makes an offer: an id no offer of the
    plan has (409 `offer_exists`), a label, the model, and the key, tried
    with the model as `keys/test` tries one, within its allowance (422
    `key_test_failed` with the provider's status and code, unless
    `skip_key_test`, which leaves the key untried); a model the lists do
    not allow is `model_denied`, one the price table does not price under
    a quota in dollars `offer_not_priced`. `PATCH …/offers/{id}` changes
    its label, whether it is on, its model (the key then untried with it,
    unless a key is given), and its key (tried and sealed, the one before
    destroyed); another provider's model needs a key (`key_required`).
    `DELETE …/offers/{id}` destroys it with its key, and says how many
    agents were on it. Both take `If-Match` (412 at another version).
    `runtime.school`'s offers are `offer_read_only` (403); an id of none,
    `offer_not_found`. Audited as `school_offer.create`, `.update` and
    `.delete`, with the key's hint and the trial's result.
  - `PUT /admin/school-plan/quotas` sets every quota, in answers a UTC day
    from 1 to 1,000,000, `per_day` null for none, and in dollars
    (`per_owner_day_usd`, `per_asker_day_usd`, `per_day_usd`: a decimal
    string or number, more than 0, at most 1,000,000, to six places; null
    for none; left out, as in force); `DELETE` takes `runtime.school`'s
    again. Both answer the plan, whose quotas carry the dollars as
    decimals with six places. Audited as `school_quotas.update` and
    `.reset`.
  - `GET /admin/prices`: the table in force, its version, the file's and
    the site's, when the site's last changed; every row, the site's first
    (`source: "site"`) then the file's (`"file"`), each with its id (a
    file's row without one by its index), provider, model, whether it is a
    glob, `from`, its prices in dollars per million tokens as exact
    decimals, and the version a cost it prices is recorded under; of a
    file's row, whether a site's row stands before it (`overridden`); of a
    site's, its version (an ETag), and who made and changed it; and the
    plan's offers, on or off, no row prices today (`unpriced_offers`).
    `GET /admin/prices/{id}` is one. `POST /admin/prices` adds a site's
    row, held to what the price file's rows are held to (an id of
    letters, digits, `.`, `_` and `-` beginning with a letter or digit, a
    provider as the ledger names it, a model of no spaces, a day, input
    and output prices, the cache's the input's unless given, to six
    places); an id or a provider, model and `from` the site's rows have is
    409 `price_exists`. `PATCH …/prices/{id}` changes one by merge-patch,
    but its id, which names it in the ledger; `DELETE` destroys one and
    answers the table. Both take `If-Match`; the file's rows are
    `price_read_only` (403), an id of none `price_not_found`. A change that
    moves a row off a model, or deletes it, is held to the quotas in
    dollars (above). Audited as `price.create`, `.update` and `.delete`.
  - `GET /admin/tenants?after=&limit=`: the tenants the runtime knows
    (`runtime.tenants`', the site's, and its agents'), by id, at most 500
    a page (100 unless `limit`), each with, where it is a hosted agents'
    owner (`ten_<actor id>`), their actor id and name as last seen; the
    quota in force and where it comes from (`site`, `config`, `none`),
    `runtime.tenants`' beside it, and how many agents are its. `GET
    /admin/tenants/{tenant_id}` is one. `PUT` sets the site's quota,
    `{"per_day": {"answers", "usd"}}`, both given, each null for none but
    not both; `DELETE` takes `runtime.tenants`' again. Audited as
    `tenant_quota.update` and `.reset`.
  - `GET /admin/agent-budgets`: the hosted agents' daily budgets by
    default in force, per agent and per asker, each `answers` and `usd`,
    `runtime.defaults`' beside them, and whether the site sets them. `PUT`
    sets both, each given, null for none; `DELETE` takes
    `runtime.defaults`' again. Audited as `agent_budgets.update` and
    `.reset`.
  - `GET /admin/costs?since=&until=&group=&key_source=&limit=&after=`:
    what the ledger recorded in a span of UTC days (`since` to `until`,
    both counted; the last 30 days to today unless given; at most 366),
    as a whole and by `day`, `tenant` (a person's with their name),
    `agent` (with its name while it is configured), `model` (key source,
    provider and model, with the plan's offers of that model now, on the
    school's key), `key_source` (the school's key or the owners' own) or
    `total`, on `school` or `own` alone when asked, a page of at most 500
    groups (100 unless `limit`) after the key `after` names, with the next
    page's cursor. Each sum is a cost in dollars and lines by kind of
    cost: `model_calls` (calls, those no price held, tokens, cost), and
    `transcription`, the transcriber's model calls (the same measures), a
    line of its own kind, so that a front end shows the lines it knows and
    the total of all. By agent, the transcriber's calls are the group
    `transcription` (`agent_id` null); by tenant, `site` (`tenant_id`
    null); by key source, the school's. The ledger has no offer's id on a
    model call: the offers of a model are those of the plan now.
  - `GET /admin/settings`' `transcription`, and `PATCH` of
    `{"transcription": {"enabled", "offer", "max_pages", "per_day_pages",
    "concurrency"}}` by merge-patch (§12): whether the transcriber can run
    here (`available`, and if not `unavailable_reason`, `operator_off` or
    `core_too_old`, with `unavailable_detail`), the site's setting or its
    defaults (off, no offer, 300 pages, no daily limit, 2 at once), where
    the offer stands in the plan in force (`offer_status`: `ok`,
    `not_found`, `disabled`, `no_file_input`, or `not_priced`, a warning),
    the credential (`status`: `none`, `ok`, `untested`, `rejected`; its
    hint, Core's id of it, who gave it when, when Core last took it, why
    it refused it), what it does now (`state`: `off`, `running`,
    `standby`, `blocked`, with `blocked_reason`: `no_credential`,
    `credential_rejected`, `no_offer`, `offer_unavailable`,
    `quota_exhausted`), and the UTC day's pages, documents done, failed and
    skipped (a job each, a file of a version since AIShie-Core #49), and
    cost. It is not turned on where it cannot run (422
    `transcription_unavailable`); an offer the plan has not (runtime.yaml's
    or the site's, on or off) is `invalid_field`, one whose model takes no
    files 422 `offer_no_file_input`; `max_pages` is 1 to 5,000,
    `per_day_pages` 1 to 1,000,000 or null, `concurrency` 1 to 8, null
    taking the default. A later change of the plan that withdraws the
    offer is not refused: `offer_status` says so, and the state is
    `blocked`. Audited as `transcription_settings.update`, with the
    members changed (a patch of `ocr` and `transcription` records
    `settings.update` too).
  - `PUT /admin/transcription/credential`, `{"token", "credential_id"?,
    "skip_test"?}`: Core's service token (`aissvc_…`, else `invalid_field`
    at `/token`), tried with Core unless `skip_test` by a call that claims
    nothing (the renewal of a claim of the nil uuid under a lease of
    chance, of the nil uuid's file where Core's renewal takes `file_id`,
    which it requires since AIShie-Core #54, and of none where it does
    not, which Core answers 404 or 409 to the service's token): Core's
    401, or 403 `service_only`, is 422 `credential_rejected` with Core's
    status in `details.status`; a Core that cannot be reached, 503
    `core_unavailable`; a Core without the service, or no `CORE_BASE_URL`,
    422 `transcription_unavailable`; nothing is kept on a refusal. Sealed
    (§11.1) under the tenant `site`, in place of the one before, which is
    destroyed; never shown, logged or audited but as its hint. `DELETE`
    forgets it (the front end revokes it in Core). Both answer the
    transcription's settings. Audited as `transcription_credential.set`
    (its hint, Core's id, whether it was tried) and `.delete`.
  - `GET /admin/transcription/jobs?after=&limit=&status=`: the
    transcriber's record of its jobs (§12), newest first, a page of at most
    200 (50 unless `limit`), of one status or all, and the next page's
    cursor (`next`, the last job's sequence); ids (a job's `file_id` and
    `position`, the file of its version it was, null from a Core before
    AIShie-Core #49), counts, the offer and model, cost and tokens, never a
    title, a file's name or any text.
  - `GET /info`'s `features.transcription`: this worker's transcriber runs,
    or stands by, as the site's setting in force turns it on, with nothing
    blocking it; what the front end shows the text versions' queue for.

### 11.6 Hosting by an agent's id

Since AIShie-Core #52 every agent is hosted one way in Core, chosen
when it is made and never changed: `runtime`, which the site's agent
runtime runs and people in the site may ask, and whose one token is the
runtime's; or `mcp`, which its owner's own tools reach over MCP with tokens
the owner issues, and which nobody asks in the site. The runtime hosts
`runtime` agents alone, by their ids, and is the only runtime that does:
nobody runs a runtime of their own for an agent, since only the site's,
holding Core's `agent_runtime` credential, is issued an agent's token, and
an `mcp` agent is never hosted here.

- **The runtime's own credential.** The runtime is a site service of
  Core's, `agent_runtime`, with a credential of its own (`aissvc_…`), which
  Core takes at the service's REST routes alone, its four of hosting and,
  since Core's migration 0026, its five of renditions (§13) (never over
  MCP, and never any other tool): `CORE_SERVICE_CREDENTIAL`, a reference
  (`secret://…`, `env://…` or `file://…`; never `sealed://`, since it is the
  operator's), `secret://core/agent_runtime` by default, the file
  `core/agent_runtime` under `SECRETS_DIR`, where Deploy writes it at setup
  (`aishie-core service issue agent_runtime --label runtime --replace`).
  `run` checks the reference at start as it checks every setting, and says
  in its log when the credential cannot be read (no agent then runs, each
  in state `error`, reason `runtime_misconfigured`, tried again after a
  backoff); it is read at each call, so a new one in its place is taken
  with no restart. Core refusing it (revoked, expired, another service's,
  or an agent's token) is `runtime_misconfigured` too, never repeating it.
  Its calls are paced by one bucket per process, 300 a minute in bursts of
  50, the worker's and the API's together: Core allows the service 600 a
  minute in bursts of 100, every worker's together, and the first start
  after an upgrade issues every hosted agent a token at once.
- **The service's calls** (`core.RuntimeService`, over `RuntimeCaller`, a
  REST caller with the credential): `check_owner` (does the person own the
  agent, and the agent as Core hosts it), `agent` (the agent: its hosting,
  standing, owner's standing, live seats, whether it is hostable and why
  not, the runtime token Core holds for it, and whether people may ask it),
  `issue_token` (the agent's one token, under a fresh idempotency key each
  time: Core's replay of a call carried out before comes back without the
  token, and is issued again under another key, which revokes the token
  never received), and `revoke_token`. None of them is offered to a model
  (`agent_runtime_*` is on the built-in deny list, §4).
- **One token per agent.** Core holds at most one live runtime token for
  an agent, and issuing one revokes the one before: two runtimes, or two
  workers, each issued one would revoke each other's. So only the worker
  holding the agent's lease is issued it, and it keeps it where every
  worker sharing its store finds it (§5.1): a hosted agent's in its row
  (`token_secret_id`, `token_issued`, `token_credential_id`), an operator's
  agent's in `agent_token` (migration 0013), each sealed, its tenant the
  owner's or the operator's (`operator` unless the configuration names
  one). An operator's agent's is held in the worker's memory without a
  keyring, issued again when the agent starts on another worker or after a
  restart. Workers that share no store must not run one agent.
- **When the hosting ends**, the token is revoked in Core: the owner
  deleting or pausing the agent (the API, after its write), its owner of
  record not its owner in Core (`owner_changed`), the agent suspended, an
  operator's agent paused or taken out of the configuration (the worker
  holding its lease, before it lets the lease go), or named as another agent
  in Core. A token revoked elsewhere (its owner, an administrator, a
  migration) is a 401 to the worker, which stops the agent until a new one
  is asked for (§5.1).
- **Operators' agents by id.** A YAML agent names its agent in Core with
  `core.agent_id` (a UUID; two agents of the configuration may not name
  one), and the runtime is issued its token as it starts it. `core.token_ref`
  is refused, saying to give `core.agent_id` in its place and to delete the
  token's file: the runtime takes no token from anyone.
- **The upgrade** from a runtime that took pasted tokens. Core's
  migration 0025 made every agent whose site chat a live token of its own
  declared `runtime`, that token the runtime's, and every other `mcp`.
  This runtime's migration 0013 lets a row hold no token, and adds
  `token_issued` (false for every row from before) and `agent_token`. At
  each hosted agent's first start after it, the worker reads it in Core,
  and is issued its token in place of the pasted one, which that revokes;
  the calls are paced by the service's bucket. A row of an agent Core made
  `mcp` is not run, in state `error`, reason `mcp_agent`, saying that an
  `mcp` agent cannot be hosted here: its owner deletes it. YAML agents must
  be given `core.agent_id` before the upgrade starts, or the configuration
  does not load. 0013 is additive, but a runtime from before cannot read a
  row that holds no token, which only this one writes, nor be given an
  owner's token for a `runtime` agent: going back past it is restoring the
  deploy's backup, with Core's own rollback (`docs/deploying.md`).

## 12. The transcriber

`internal/transcribe` gives every file of a course's documents its text
version in Core (AIShie-Core #43; §4, Text versions): the file transcribed
into Markdown by a model of the school's plan. A version holds several
files since AIShie-Core #49, and each has a text version of its own: Core's
queue hands out files, not versions, and the transcriber works file by
file (The loop, below); a Core before it hands out versions of one file,
which are worked on as they always were. It is a
module of its own, off unless the site's administrators turn it on
(§11.5); off, the runtime claims nothing from Core's queue, and nothing
else in it behaves differently. It is the one place the runtime writes to
Core as anything but an agent: as the site's transcription service, an
actor of Core's that is in no course, with a credential that works at the
service's four REST routes alone (`document_text.queue`, `.file`,
`.renew`, `.complete`), which an administrator issues in Core and hands to
the runtime through the API.

- **Where it runs.** The operator's environment is its ceiling:
  `TRANSCRIBE=auto` (the default) lets it run when the site turns it on;
  `off` never, whatever the site says (`available: false`,
  `operator_off`); `on` as `auto`, and `run` and `check` fail where it
  cannot run. It needs the store in PostgreSQL, the key that seals
  secrets and `CORE_BASE_URL` (else `operator_off`, saying which), and a
  Core whose catalogue has the service (else `core_too_old`, the
  catalogue read again every 10 minutes). `check` says where it stands;
  `run` starts it, and its start's log line says so.
- **One claimer.** Every worker runs a `transcribe.Service`; the one that
  holds the store's lease `transcriber` (a minute, renewed every 20 s)
  claims, the others stand by, and one takes over within moments of its
  lapse. N replicas never multiply the concurrency. Turned off, the lease
  is given up.
- **What it pays with.** One offer of the school's plan (runtime.yaml's or
  the site's), which the administrators choose, on the school's key: its
  model is made as an agent's on it would be, over the runtime's client
  for runtime.yaml's and the hosted-model client for the site's. Its
  calls are the ledger's, of their own kind (`transcription`, §8): no
  agent's, tenant's, course's or asker's, counted against the plan's
  ceiling across the key (`per_day_usd`) and no other quota, and a line
  of their own in the cost report. An offer whose model takes no files
  cannot transcribe: a PDF goes to a model whose API and provider take
  PDFs (OpenAI's own and Azure's, Anthropic's, Gemini's, OpenAI's
  Responses, Bedrock's Converse), and to one that takes pictures and no
  PDFs, each page drawn by pdftocairo at 150 dpi.
- **The loop.** While it holds the lease and nothing blocks it, it asks
  the queue for as many files as it has free slots (`concurrency`),
  each claim 10 minutes (`lease_s: 600`), the call waiting up to 25 s for
  one (`wait_s`); Core hands a version's files out in their order, each
  a claim of its own (its `file_id`, `position` and `filename`, and the
  file's type, size and URL), and each file is its own job, whatever
  becomes of its version's others. Every call the transcriber makes of a
  claim names its version and its file (`document_text.file`, `.renew`
  and `.complete` with `file_id`), and its completion's key is the
  file's, `complete:{file_id}:{lease_id}`; a claim of a Core before #49
  names no file, and none is sent, the key then
  `complete:{version_id}:{lease_id}` as before. It works on each claimed
  file in the background: it
  fetches the file from its signed URL, with no credential (a fresh URL
  from `document_text.file` where it has expired or is refused); knows it
  by its type or, of no telling type, by what it holds; a text file
  (`text/plain`, Markdown, CSV) is done with its own text, `model: "text
  file"`, no model called; an image is one page; a presentation or a
  document is converted to PDF by LibreOffice (`internal/office`), a deck
  of PowerPoint's with its speaker notes read beside it and given to the
  model with its slides; a PDF's pages are counted. It asks the model for
  a range of pages at a time (`PDF_PART_PAGES`, 10, within what the
  provider takes; 5 as pictures), each range a PDF of its own cut by the
  pager (the whole PDF where none cuts, within the provider's pages),
  halving a range whose text the output bound cut off, and joins the
  ranges' texts. It renews the claim every third of it meanwhile, and
  completes the file, under the key of its claim, done with
  the text, its pages and the offer's label as the model, or failed or
  skipped with why; a completion Core cannot be reached for is sent again
  under its key. Failures that may pass (rate limits, overload, the
  network) are tried three times a range, with backoff; then the file
  fails (`model_error`).
- **The prompt** is a constant (`transcribe.Prompt`), the same for every
  document and range: transcribe faithfully, in the document's own
  language, nothing summarised, translated or added; each page under
  exactly `## 第 N 頁`, a slide under `## 投影片 N`, N counting from 1
  across the whole document; structure kept (headings, lists, Markdown
  tables, LaTeX, fenced code with its language); each picture, diagram,
  chart or screenshot one bracketed line, `[圖：…]`, saying what it shows;
  a slide's speaker notes after it, `> 講者備註：…`; what cannot be read
  `[無法辨識]`; no preamble, no closing remarks. The headings are Chinese
  whatever the document's language: the front end and the models'
  citations find pages by them. A model's code fence around its whole
  answer is taken off, and a range whose text begins without its first
  heading is given it.
- **What is not transcribed**, and Core is told why: a document of more
  pages than `max_pages` (`too_many_pages`); one whose pages do not fit in
  what the day's `per_day_pages` leave (`quota_exhausted`); a file that
  needs a password (`encrypted`); a type not transcribed, a workbook, an
  Office file where LibreOffice is not (`unsupported_format`); a file of
  more than 64 MiB (`too_large`); an empty text file (`empty`). A
  conversion that fails is `conversion_failed`. A text past the 2 MiB
  Core keeps is cut before the last page heading that fits, and ends with
  the line `[本文過長，其餘頁面未收錄]`: still done.
- **Blocked.** It claims nothing, and says why (`blocked_reason`), with no
  credential, or one Core refused (401, or 403 `service_only`: it is not
  tried again until another is given); no offer, or one the plan no longer
  offers or whose model takes no files (`offer_unavailable`); the day's
  pages all spent, or the plan's dollars across the school's key
  (`quota_exhausted`, until the next UTC day).
- **Dropped work.** Core refusing a renewal or the completion because the
  claim was lost (`lease_lost`: a retranscription broke it, or it lapsed),
  staff wrote the text meanwhile (`edited_by_staff`), the course or the
  document was archived, or the version is gone, stops the work, and
  nothing is written; so does the worker stopping (its claims lapse, and
  the files are claimed again). Core says each of the file the call
  named: one file's claim lost, or its text written by staff, drops that
  file's work alone, and its version's other files go on. A job a worker left `working` for
  longer than a claim is ended as `dropped` (`interrupted`) by the next
  claimer.
- **Without a restart.** The site's setting and the credential are kept in
  the store; every write moves `registry_rev` on, and each worker's build
  puts them in force (`transcribe.Service.Set`), from the next claim: work
  under way goes on as it began.
- **What it keeps** (§8): the credential, sealed, and a record of each job
  (a claim: a file, by its `file_id` and `position`, none from a Core
  before #49) for 90 days, pruned by the claimer. **What it counts:**
  `transcribe_jobs_total{outcome}` (done, failed, skipped, dropped),
  `transcribe_pages_total` (pages sent to the model),
  `transcribe_inflight`, `transcribe_claim_errors_total{reason}`
  (unauthenticated, rate_limited, unreachable, refused), and the model
  calls' own metrics, on the school's key. It logs ids, counts and codes:
  never the token, a key, a file's URL or any text.

## 13. The renditions worker

Every Office or OpenDocument file Core keeps, of a document's version of
any kind (material, instructions, a rubric, a submission, feedback) or
carried by a message, is previewed in the site as a PDF, made once, on the
server, by the site's runtime (AIShie-Core's migration 0026, its
docs/schema.md §2.4 *Renditions*). Core queues a rendition as it records
the file, and backfills what it kept before; `internal/rendition` takes
them from Core's queue, converts them with LibreOffice, and hands the PDFs
back. There is no AI in it and no cost to the site, so no switch of the
site's: it is plumbing, on by default.

- **Which files.** Core's one table (`file_rendition_convertible`), which
  the runtime holds the same (`rendition.Convertible`, held to the
  contract's table and examples by a test): a name whose extension (after
  its last dot, case aside) is `doc`, `dot`, `docx`, `docm`, `dotx`,
  `xls`, `xlt`, `xlsx`, `xlsm`, `xltx`, `ppt`, `pps`, `pot`, `pptx`,
  `pptm`, `ppsx`, `potx`, `odt`, `ods`, `odp`, `odg` or `rtf`, and a
  declared type (before its parameters, case aside) that is one of the
  Office, OpenDocument and RTF types or says nothing of the bytes
  (`application/octet-stream`, `application/zip`,
  `application/x-zip-compressed`, `application/vnd.ms-office`); any listed
  type with any listed extension. Never a PDF, a picture, a text or an
  archive. Each extension names the format LibreOffice is given the file
  in: a document by Writer's PDF export, a presentation by Impress's (a
  page a slide, the hidden ones too, no notes pages), a workbook by
  Calc's, a drawing (`odg`) by Draw's.
- **Where it runs.** `RENDITIONS=auto` (the default) runs it wherever
  LibreOffice converts here (`OFFICE_PDF` not `off`, `soffice` and
  `prlimit` installed, as in the image) and `CORE_BASE_URL` is set; `off`
  never; `on` as `auto`, and `run` and `check` fail where it cannot run, a
  Core whose catalogue has no renditions among the reasons (`core_too_old`,
  the catalogue read again every 10 minutes). It calls Core as the site's
  `agent_runtime` service, with the runtime's own credential (§11.6,
  `CORE_SERVICE_CREDENTIAL`), read at each call, through the bucket every
  call of that service in the process shares (300 a minute, bursts of 50):
  nothing else to configure. `check` and the start's log line say where it
  stands.
- **The loop.** It asks for as many files as it has free slots
  (`RENDITIONS_CONCURRENCY`, 1), each claim `RENDITIONS_LEASE` (10
  minutes), the call waiting up to 25 s for one (`wait_s`), and works on
  each in the background: it fetches the file from its short-lived URL,
  no credential and no redirect followed, at most 100 MiB (a larger one is
  skipped `too_large` unread), from a fresh URL (`rendition_file`) where it
  has lapsed or is refused; holds it to what LibreOffice converts (an
  Office Open XML file encrypted in its container, or an OpenDocument file
  whose manifest gives its parts encryption data, is skipped
  `password_protected`; bytes held neither as a zip, a Compound File nor
  RTF are skipped `unsupported`); converts it, a PDF of every page with no
  page cap, in the sandbox of every conversion (§4, Office files: prlimit,
  no network, a fresh profile, macros off), within `RENDITIONS_TIMEOUT` (5
  minutes; past it, failed `timeout`), as large as Core's `max_bytes` takes
  (past it, skipped `too_large`), LibreOffice failing or making nothing
  being failed `conversion_failed`; and renews the claim every half of its
  lease meanwhile. A PDF made is put at the upload URL Core gives for the
  claim (`rendition_upload_url`), with exactly its headers and no
  credential, and the rendition completed done with its pages, under the
  key Core suggests, `rendition:{rendition_id}:{lease_id}:{n}`, sent again
  under it while Core does not answer; one Core refuses as no PDF or as
  too large is completed failed `conversion_failed` or skipped
  `too_large` under the next `n`; one Core says never arrived is put once
  more at a new URL. Failed and skipped are completed under the key too.
- **Dropped work.** Core saying the claim is lost (`lease_lost`: it lapsed
  and was claimed again, or the credential that made it was revoked), the
  file gone (purged), or the credential refused, stops the work: the
  conversion is killed, and nothing is uploaded or completed under that
  claim. A file that cannot be fetched, a PDF whose upload nothing
  answers, a completion Core never answers, and the worker stopping leave
  the claim to lapse: Core gives the file
  out again, and fails it `attempts_exhausted` after five claims.
- **Blocked.** No credential (none readable, or not a service's), or one
  Core refuses (401, `service_only`, `not_for_services`), or a site that
  keeps no files (`no_file_storage`), stops the claiming, logged once; it
  is tried again every minute, and a credential put in its place is taken
  at the next try, with no restart.
- **Several workers.** No lease of the store's elects one, as the
  transcriber's does (§12): Core never gives one file to two claims, there
  is no quota or cost of the site's to share, and LibreOffice's load is
  each host's. Every worker converts as many at once as its own
  `RENDITIONS_CONCURRENCY`, and stopping one leaves its claims to lapse.
- **What it keeps and counts.** Nothing in the store: Core keeps the
  queue, the PDFs and the record (each completion is an action of the
  service's). It counts `rendition_jobs_total{outcome,reason}` (done,
  failed, skipped, dropped), `rendition_seconds`, `rendition_inflight` and
  `rendition_claim_errors_total{reason}` (no_credential,
  credential_rejected, rate_limited, unreachable, refused). One log line a
  file: the rendition's, course's and file's ids, the attempt, the
  extension, sizes, pages, outcome, reason and time; never the file's
  name, a URL, an upload's token, the credential or what the file holds.
- **What the models read**: a model given an Office file whose rendition
  is done is given that PDF, which the runtime reads of Core as any
  reader of the file does, with the agent's own token, and fetches from
  the URL Core gives every reader, never with the service's credential;
  while it is not, and where Core has
  none, the worker converts the file itself, as §4 (Office files, Core's
  PDF first) says. `document_rendition_retry` and the rendition tools are
  on the built-in deny list (§4).
