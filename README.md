# AIshie Agent Runtime

The runtime that hosts AI agents for the AIshie LMS, the site's one
runtime. It connects in to
[AIshie Core](https://github.com/AIShie-Education/AIShie-Core) as each
agent Core hosts `runtime`, named by its id, with the one token Core
issues the runtime for it (nobody pastes a token; an `mcp` agent is its
owner's tools' to reach, never the runtime's). It finds the questions put
to the agent and answers them with the model its owner chose, calling
Core's tools only as far as the agent's seat allows.

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
- **Agents read the course's documents.** The runtime, never the model,
  fetches a document's file and gives it to the model by what it is: a
  presentation or a document (`.pptx`, `.ppt`, `.odp`, `.docx`, `.doc`,
  `.odt`, `.rtf`) as its PDF, the one Core keeps of it where Core has made
  it and else the one LibreOffice makes here, slides as they look
  with their speaker notes beside them, where the model takes files, and
  as its text where it does not, slides, notes and tables kept apart, with
  what OCR reads of the slides' pictures; a workbook as its text, sheet by
  sheet; a PDF as a file where the model takes files, in parts of ten
  pages when it is long, and as its text where it does not; a scan, and an
  image, to a model that takes no files as the text the runtime's OCR
  recognizes, in Chinese and English, marked as such. Every file is read
  as a hostile one, within fixed limits, and LibreOffice and the OCR's
  programs run apart, held in memory, time and what they see. Where Core
  has a file's text version (文字版), the model reads that first, marked as
  an AI transcription or the staff's, and may ask to see pages of the file
  to check one.
- **Agents search the course's materials.** A tool of the runtime's own,
  `course_materials_search`, finds the passages of the course's documents
  that best match a few words, in Chinese or English, and says where each
  is (the document, version, file, and slide or page) with an excerpt and
  the call that reads it, so that a model need not read whole decks to
  find one. It searches only what the asking seat may read, as Core gives
  it that seat, from an index the runtime builds in its own store as
  searches need it, and drops as it hears of a version's purge, or after
  30 days unused; no PostgreSQL extension is needed.
- **Answers say what they relied on.** A posted answer names the course
  materials its model was given while it wrote it, in the order it read
  them: each version of a material, instructions or rubric that its
  `document_get` gave it some of, with the file, the page or slide, and
  the part of the text version as Core numbers it, where the runtime knows
  them. An answer that read none says so, and one that only saw a search's
  excerpts says nothing. Core keeps them with the answer and shows each
  reader what they may open
  ([`docs/design.md`](docs/design.md#53-one-answer), What the answer
  relied on).
- **Text versions of the course's files.** A module of its own, off until
  the site's administrators turn it on: one worker at a time claims the
  files waiting in Core's queue, has a model of the school's plan
  transcribe each into Markdown, page by page, with its pictures
  described, and writes the text back to Core, on the school's key, its
  costs a line of their own.
- **Office files previewed as PDF.** On by default, with nothing to
  configure but the runtime's own credential in Core: every Office and
  OpenDocument file Core keeps, of a document or a message, is converted
  once by LibreOffice, in the same sandbox, to the PDF the site previews
  it as, taken from Core's queue and handed back to Core
  ([`docs/deploying.md`](docs/deploying.md#office-files-previewed-as-pdf-renditions));
  a model reading the file is given that same PDF, and the file is not
  converted again.
- **Five model APIs.** OpenAI Chat, which also covers the compatible servers
  (DeepSeek, Azure OpenAI, Ollama, vLLM…), OpenAI Responses, Anthropic,
  Gemini and Bedrock Converse. Each agent can have a fallback model.
- **Safe answers.**
  - Answers are exactly once. Each is written ahead under Core's idempotency
    key, `answer:{conversation}:{message}:{attempt}`, so no crash, retry or
    second worker posts it twice.
  - Proposals are followed. An agent whose seat is at `confirm_required`
    proposes. It notes the answer when a person approves, and tries again
    with the reason when one rejects.
  - Conversations stay open. The runtime never closes one: a question it
    could not answer in its attempts waits until the next day.
  - Stop means stop. A question its asker withdraws (retracts) is not
    answered: the answer being written to it stops at once, its model call
    cancelled, and nothing is posted.
  - No answer is cut off unexplained. One that runs past the model's
    output cap is continued where it stops, within the answer's budgets,
    the last continuation told to bring it to a close; one still cut short
    says so, and that a reply of "continue" brings the rest.
  - Budgets and quotas: per answer (turns, tool calls, tokens, wall clock),
    and per asker, agent and tenant per day, in answers and dollars.
  - Nothing leaks out. Links and images that could carry data out are
    stripped from what the model writes. Tokens and keys are redacted from
    every log.
- **Core's rate limit is shared.** Each agent polls within its share of it.
  Where Core's reads wait for news, each inbox call waits for a question
  and a question is noticed within milliseconds; against an older Core the
  inbox is polled faster while a conversation is active, backing off when
  idle.

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
quotas that hold them. The runtime's administrators also turn OCR off and
on, choose its languages, turn the transcriber on and hand it Core's
service credential, make offers of the school's plan, set its quotas,
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
- [`runtime.yaml`](examples/runtime.yaml), with the school's plan, and
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
| `CORE_SERVICE_CREDENTIAL` | where the runtime's own credential in Core is (its `agent_runtime` service's, `aissvc_…`), with which it is issued each agent's token by the agent's id, and takes the Office files it converts to PDF: `secret://core/agent_runtime` by default ([`docs/deploying.md`](docs/deploying.md#the-runtimes-own-credential-in-core)) |
| `SECRETS_DIR` | where `secret://` references are looked for |
| `PRICES` | the price table, instead of the runtime's `prices_ref` |
| `KMS_KEY_ID` | the key that seals the secrets kept in the database: `local:<dir>/<name>`, a 32-byte key in that file |
| `API_ADDR`, `API_AUDIENCE` | where the JSON API for the front end listens, apart from `HTTP_ADDR` (unset, there is none), and the audience Core's assertions name for this runtime, as Core's `RUNTIME_AUDIENCES` lists it ([`docs/design.md`](docs/design.md) §11.4) |
| `CORE_ASSERTION_KEY`, `ADMIN_ACTOR_IDS`, `API_TRUSTED_PROXIES` | Core's assertion key, pinned; the runtime's administrators among Core's; the proxies in front of the API |
| `EGRESS_PROXY` | a proxy for every outbound call |
| `LOG_LEVEL`, `LOG_FORMAT` | `info` and `json` by default |
| `LOG_REDACT_EXTRA` | regular expressions redacted from logs, beside the built-in token and key shapes |
| `WORKER_ID`, `SHUTDOWN_GRACE` | this process's name in leases (default hostname-pid); the grace on `SIGTERM` (default `15s`) |
| `OCR`, `OCR_*` | the OCR of scanned PDFs and images for models that cannot take the files: `auto` (on where tesseract, pdftoppm and prlimit are, as in the image), `on` or `off`, and its languages, pages, resolution, time, memory and turns ([`docs/deploying.md`](docs/deploying.md#scanned-documents-ocr)) |
| `OFFICE_PDF`, `OFFICE_PDF_*`, `PDF_PART_PAGES` | the conversion of presentations and documents to PDF by LibreOffice: `auto` (on where soffice and prlimit are, as in the image), `on` or `off`, its time, pages and memory; and the pages of a PDF a model is given as one file ([`docs/deploying.md`](docs/deploying.md#presentations-and-documents-libreoffice)) |
| `RENDITIONS`, `RENDITIONS_*` | the PDF renditions of the Office files Core keeps: `auto` (on wherever LibreOffice converts and `CORE_BASE_URL` is set), `on` or `off`, how many at once, each one's time, and Core's lease on it ([`docs/deploying.md`](docs/deploying.md#office-files-previewed-as-pdf-renditions)) |

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

Go 1.27 and PostgreSQL 13 or later. `make help` lists the targets:

- `make test` runs the unit and integration tests. They run against a fake
  Core held to fixtures recorded from the real one, and against scripted
  models. They also use the store on `TEST_DATABASE_URL`.
- `make e2e` starts the Core pinned in `.github/core-image` and runs `e2e/`
  against it.
- `make lint` runs gofmt, go mod tidy, actionlint, go vet and golangci-lint.
- `make live` tries the adapters against the real providers whose keys are
  set, and `make live-core` tries Core's client and a seat's toolset
  against a Core of their own. CI runs neither; `docs/design.md` §10 says
  when to.

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
runtime's own settings (the agents and their secrets, OCR, LibreOffice, the
renditions, hosted agents, the school's plan, the API) holds in the stack
as well.

## License

AIshie Agent Runtime is copyright 2026 XIE Hanming, and source-available under the [Elastic License 2.0](LICENSE) (ELv2), governed by the laws of Hong Kong. You may use, copy, change and redistribute it on the terms in LICENSE, which include that you may not offer it to others as a hosted or managed service.

For clarity: an educational institution that runs its own installation for its own staff and students is not providing the software to third parties as a hosted or managed service.

（補充說明：教育機構自行架設、供其教職員及學生使用，不視為向第三方提供託管服務。）
