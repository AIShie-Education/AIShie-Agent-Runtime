# AIshie Agent Runtime

The runtime that hosts AI agents for the AIshie LMS, the site's one
runtime. It owns the browser's conversations, messages, attachments, drafts,
proposals and SSE in its own PostgreSQL store; Core stores none of that chat
state. A Go control plane keeps hosting, assertions, Vault, leases, quotas,
pricing and write-ahead recovery, and supervises a Node 22 TypeScript runner
on loopback. The runner uses Vercel AI SDK and connects to
[AIshie Core](https://github.com/AIShie-Education/AIShie-Core) as each
agent Core hosts `runtime`, named by its id, with the one token Core
issues the runtime for it (nobody pastes a token; an `mcp` agent is its
owner's tools' to reach, never the runtime's). Core is called over MCP only
as far as the agent's current seat allows.

Core is the contract. Its handout, `docs/agent-runtime.md` in AIShie-Core,
says what an agent may do and how. This repository meets it:

- **Two kinds of agent.** A student's or instructor's own agent is a delegate
  that answers only its owner. A course tutor answers every student in a
  course.
- **Agents act for their owners.** In a conversation its owner opened, an
  agent's model may make the changes its seat's permissions allow, such as
  writing a document, as Core decides them: done at `autonomous`, waiting
  for a person's approval at `confirm_required`, refused at `denied`. Anyone
  else it answers gets reads alone. Each change is keyed so that no retry
  makes it twice, and the agent tells its owner plainly what it did.
- **Agents read the course's documents.** The runner calls Core's native
  `document_get` and `document_text` tools, including a file's staff-written
  text version, within the asking seat's current permissions. Conversation
  attachments are deliberately lightweight: at most four images of at most
  5 MiB each and small `text/*` files go directly to a multimodal model.
  AR does no OCR, Office conversion, PDF splitting, transcription or
  rendition, and the image contains no LibreOffice, Tesseract, Poppler or
  CJK font toolchain.
- **Agents search the course's materials.** Core's native
  `course_materials_search` finds matching passages in Chinese or English and
  returns the document, version, file, page or slide, part and a bounded
  excerpt, plus the exact read call. Core keeps a zero-extension GIN index,
  updates it in the writing transaction and backfills older material in the
  background. Every search applies the live course, publication, assignment
  and principal scopes; no PostgreSQL extension is needed.
- **Answers say what they relied on.** A posted answer names the course
  materials its model was given while it wrote it, in the order it read
  them: each version of a material, instructions or rubric that its
  `document_get` gave it some of, with the file, the page or slide, and
  the part of the text version as Core numbers it, where the runtime knows
  them. An answer that read none says so; one that only saw a search's
  excerpts says nothing, as does one that read none after an earlier
  answer in the conversation that relied on some, or a revision that
  read none of an answer sent back for changes that did. A notice of the
  runtime's own (a spent budget or quota, a refusal) is no answer, and
  says nothing of sources either. A source Core
  refuses (archived or purged since it was read) is dropped and the same
  answer posted again at once, without asking the model again or using up
  one of its attempts, after a restart too. Core keeps them with the
  answer and shows each reader what they may open
  ([`docs/design.md`](docs/design.md#53-one-answer), What the answer
  relied on).
- **Five model APIs.** OpenAI Chat, which also covers the compatible servers
  (DeepSeek, Azure OpenAI, Ollama, vLLM…), OpenAI Responses, Anthropic,
  Gemini and Bedrock Converse. Each agent can have a fallback model.
- **Safe answers.**
  - AR writes each model result ahead before publishing it. Every generation
    has a holder, fencing token and lease, so a stale worker cannot overwrite
    or publish over a newer attempt. Idempotent create/ask and deterministic
    message ids keep retries from duplicating a conversation or answer.
  - Submitting for approval is a successful `proposed` result, not an error
    and not execution. A pending question does not invoke a model until it is
    approved; a pending answer's body is hidden from its asker until approval.
    Rejection, changes requested and post-publication review are recorded
    separately, with idempotent decisions and fresh Core authorization checks.
  - Withdrawal aborts the active HTTP call; the runner's AbortController stops
    the model and closes its MCP connection. A retracted question is never
    published over by a late result.
  - Per-answer limits cover steps, tool calls, writes, output and wall clock;
    daily quotas cover askers, agents and tenants in answers and dollars.
  - Links and images that could carry data out are stripped from answers.
    Tokens, provider keys, signed URLs and provider error bodies are not
    written to chat rows or logs.
- **Core remains the authorization boundary.** Every MCP call is authorized
  again by Core. The runtime does not poll a Core conversation inbox or post
  Core conversation answers; its own outbox, leases and recovery sweeps drive
  the new browser chat.

How it is built, and where it departs from the handout, is in
[`docs/design.md`](docs/design.md).

## Commands

```
aishie-runtime run                         the worker, and /healthz, /metrics, /status on HTTP_ADDR
aishie-runtime check [--live]              validate the configuration; --live reads each agent in Core,
                                           shows its seats and tools, and tries each model key
aishie-runtime migrate up                  the store's schema (PostgreSQL)
aishie-runtime migrate down --yes
aishie-runtime migrate version
aishie-runtime keys check                  every secret sealed in the store opens with the keyring
aishie-runtime keys rewrap                 wrap every secret's data key under KMS_KEY_ID's key
aishie-runtime catalogue --core URL        Core's tool catalogue and its hash;
          [--check FILE] [--write FILE]    --check exits 1 when it differs from FILE
aishie-runtime version | help
```

It exits 0 when all went well, 1 on a failure and 2 on wrong usage.
`SIGHUP` reloads the configuration and the price table. `SIGTERM` gives the
answers in progress `SHUTDOWN_GRACE` to finish.

## Configuration

Agents are YAML, one document per agent, in the shape of the handout's §4,
and, with `DATABASE_URL`, the hosted agents people host themselves by their ids,
which the runtime keeps in its database and runs beside them
([`docs/design.md`](docs/design.md) §11).
Each document holds the agent, and overrides per course if it has any.
One `runtime:` document holds the process's own settings: tenants' quotas,
the price table, the models the school's key may use, and the school's AI
plan, the models the school offers hosted agents on its own key with the
quotas that hold them; an offer of a model at OpenRouter may also say which
of the upstream providers serving it may answer, and on what terms (no
data kept, zero data retention, precision, highest price), which goes with
every call made on it. The runtime's administrators make offers of the
school's plan (one at OpenRouter with its upstream routing, set against
OpenRouter's list of the upstream providers serving its model), set its quotas,
the tenants' and the hosted agents' budgets, add prices beside the price
file's, and read what things cost, from the front end, within what the
environment and `runtime.yaml` allow
([`docs/deploying.md`](docs/deploying.md#what-the-sites-administrators-change)).
See [`examples/`](examples):

- [`agents/delegate.yaml`](examples/agents/delegate.yaml): a student's own
  agent, on the student's own DeepSeek key.
- [`agents/course-tutor.yaml`](examples/agents/course-tutor.yaml): an
  instructor's course tutor, on the school's Anthropic key, with a fallback
  model and settings for one course.
- [`runtime.yaml`](examples/runtime.yaml), with the school's plan (an offer
  at OpenRouter with its upstream routing among them), and
  [`prices.example.yaml`](examples/prices.example.yaml).

Secrets are never written in the YAML, only referred to:

| Reference | Where it is read from |
| --- | --- |
| `secret://a/b` | the file `$SECRETS_DIR/a/b`, else the variable `AISHIE_SECRET_A_B` |
| `env://NAME` | the variable `NAME` |
| `file:///abs/path`, `file://rel/path` | the file; a relative one is relative to the agent's YAML file |
| `sealed://sec_…` | a secret sealed in the runtime's own database, under the key `KMS_KEY_ID` names: how hosted agents' keys, and the tokens Core issues the runtime, are kept |

The process is set up from the environment:

| Variable | What it is |
| --- | --- |
| `DATABASE_URL` | the runtime's own PostgreSQL, never Core's. Unset, state is kept in memory, which is for one worker and for tests. |
| `CONFIG` | the YAML files and directories, separated by commas |
| `HTTP_ADDR` | where `/healthz`, `/metrics` and `/status` listen (default `127.0.0.1:9090`) |
| `CORE_BASE_URL_ALLOWLIST` | the Core origins or host patterns an agent may point at |
| `CORE_BASE_URL` | the Core the hosted agents run at: those people host themselves, kept in the database (`DATABASE_URL`) |
| `CORE_SERVICE_CREDENTIAL` | where the runtime's own credential in Core is (its `agent_runtime` service's, `aissvc_…`), with which it is issued each agent's token by the agent's id: `secret://core/agent_runtime` by default ([`docs/deploying.md`](docs/deploying.md#the-runtimes-own-credential-in-core)) |
| `SECRETS_DIR` | where `secret://` references are looked for |
| `PRICES` | the price table, instead of the runtime's `prices_ref` |
| `KMS_KEY_ID` | the key that seals the secrets kept in the database: `local:<dir>/<name>`, a 32-byte key in that file |
| `API_ADDR`, `API_AUDIENCE` | where the JSON API for the front end listens, apart from `HTTP_ADDR` (unset, there is none), and the audience Core's assertions name for this runtime, as Core's `RUNTIME_AUDIENCES` lists it ([`docs/design.md`](docs/design.md) §11.4) |
| `CORE_ASSERTION_KEY`, `ADMIN_ACTOR_IDS`, `API_TRUSTED_PROXIES` | Core's assertion key, pinned; the runtime's administrators among Core's; the proxies in front of the API |
| `EGRESS_PROXY` | a proxy for every outbound call |
| `LOG_LEVEL`, `LOG_FORMAT` | `info` and `json` by default |
| `LOG_REDACT_EXTRA` | regular expressions redacted from logs, beside the built-in token and key shapes |
| `WORKER_ID`, `SHUTDOWN_GRACE` | this process's name in leases (default hostname-pid); the grace on `SIGTERM` (default `15s`) |
| `RUNNER_URL` | an external TypeScript runner; unset, `run` manages the same-container loopback runner |
| `RUNNER_PORT`, `RUNNER_BIN`, `RUNNER_ENTRY` | its loopback port (default 3001), executable (default `node`) and script (default `runner/dist/index.js`) |

`aishie-runtime help` lists them as the binary reads them.

## Trying it

```sh
make build
CONFIG=examples/runtime.yaml,examples/agents bin/aishie-runtime check
```

To run an agent for real, start from one of the examples:

1. Make the agent in Core hosted `runtime`, and seat it in a course (handout §2.6 and §2.7).
2. Put its id in `core.agent_id`, and a model key where `key_ref` points.
3. Point `core.base_url` at your Core, and give the runtime its own credential there
   (`aishie-core service issue agent_runtime`, in the file `CORE_SERVICE_CREDENTIAL` names).

Then:

```sh
bin/aishie-runtime check --live   # shows the seats Core gave the agent
DATABASE_URL=postgres:///aishie_runtime bin/aishie-runtime migrate up
DATABASE_URL=postgres:///aishie_runtime bin/aishie-runtime run
```

## Development

Go 1.27.4, Node 22 and PostgreSQL 13 or later. `make help` lists the targets:

- `make test` runs the Go unit and store integration tests, including the
  memstore/PostgreSQL chat contracts and small httptest Core/Runner mocks.
- `make runner-ci` installs and checks the TypeScript runner, then runs its
  Vitest suite and build.
- `make e2e` starts the Core pinned in `.github/core-image` and runs `e2e/`
  against the real AR chat path.
- `make lint` runs gofmt, go mod tidy, actionlint, go vet and golangci-lint.
- `make live` probes each configured provider through the TypeScript runner;
  a provider without its key is skipped.

[`CONTRIBUTING.md`](CONTRIBUTING.md) covers how changes, migrations and
releases are made.

## Deploying

Every green commit on `main` is published to
`ghcr.io/aishie-education/aishie-agent-runtime` as `:sha-<commit>`, and moves
`:edge`. Tags `v*` are releases: the highest stable one is `:stable` too.
This repository and its image are public: anyone pulls the image, and clones
the code, with no login. The image serves no port to the internet: it
connects out, to Core and the model providers.

A site runs the runtime with Core and the web front end, one Docker Compose
stack per server, which
[AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy) sets up
and documents. The server keeps itself up to date: every five minutes it
pulls the tag each service follows, `:edge` on a test site and the release
its operator names on a school's, and deploys a new image by a safe
sequence. Nothing in this repository reaches a server.

The older way is still here: [`deploy/`](deploy) adds the runtime to a
server of Core's own, and the Deploy workflow deploys to it over SSH once
this repository has the server's address and key; until then a deploy
records itself, says which image is ready and does nothing.
[`docs/deploying.md`](docs/deploying.md) says how such a server is set up,
how the workflow is connected, and how to roll back; what it says of the
runtime's own settings (the agents and their secrets, the loopback runner,
hosted agents, the school's plan, the API) holds in the stack as well.

## License

AIshie Agent Runtime is copyright 2026 XIE Hanming, and source-available under the [Elastic License 2.0](LICENSE) (ELv2), governed by the laws of Hong Kong. You may use, copy, change and redistribute it on the terms in LICENSE, which include that you may not offer it to others as a hosted or managed service.

For clarity: an educational institution that runs its own installation for its own staff and students is not providing the software to third parties as a hosted or managed service.

（補充說明：教育機構自行架設、供其教職員及學生使用，不視為向第三方提供託管服務。）
