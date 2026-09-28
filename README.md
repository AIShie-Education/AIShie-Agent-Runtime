# AIShie Agent Runtime

The runtime that hosts AI agents for the AIShiteru LMS. It connects in to
[AIShiteru Core](https://github.com/AIShie-Education/AIShie-Core) as each
agent, with the token the agent's owner issued there. It finds the
questions put to the agent and answers them with the model its owner
chose, calling Core's tools only as far as the agent's seat allows.

Core is the contract. Its handout, `docs/agent-runtime.md` in AIShiteru-Core,
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
  - Budgets and quotas: per answer (turns, tool calls, tokens, wall clock),
    and per asker, agent and tenant per day, in answers and dollars.
  - Nothing leaks out. Links and images that could carry data out are
    stripped from what the model writes. Tokens and keys are redacted from
    every log.
- **Core's rate limit is shared.** Each agent polls within its share of it,
  faster while a conversation is active and backing off when idle.

How it is built, and where it departs from the handout, is in
[`docs/design.md`](docs/design.md).

## Commands

```
aishie-runtime run                         the worker, and /healthz, /metrics, /status on HTTP_ADDR
aishie-runtime check [--live]              validate the configuration; --live connects each agent,
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
and, with `DATABASE_URL`, the hosted agents people connect themselves,
which the runtime keeps in its database and runs beside them
([`docs/design.md`](docs/design.md) §11).
Each document holds the agent, and overrides per course if it has any.
One `runtime:` document holds the process's own settings: tenants' quotas,
the price table, and the models the school's key may use. See
[`examples/`](examples):

- [`agents/delegate.yaml`](examples/agents/delegate.yaml): a student's own
  agent, on the student's own DeepSeek key.
- [`agents/course-tutor.yaml`](examples/agents/course-tutor.yaml): an
  instructor's course tutor, on the school's Anthropic key, with a fallback
  model and settings for one course.
- [`runtime.yaml`](examples/runtime.yaml) and
  [`prices.example.yaml`](examples/prices.example.yaml).

Secrets are never written in the YAML, only referred to:

| Reference | Where it is read from |
| --- | --- |
| `secret://a/b` | the file `$SECRETS_DIR/a/b`, else the variable `AISHIE_SECRET_A_B` |
| `env://NAME` | the variable `NAME` |
| `file:///abs/path`, `file://rel/path` | the file; a relative one is relative to the agent's YAML file |
| `sealed://sec_…` | a secret sealed in the runtime's own database, under the key `KMS_KEY_ID` names: how hosted agents' tokens and keys are kept |

The process is set up from the environment:

| Variable | What it is |
| --- | --- |
| `DATABASE_URL` | the runtime's own PostgreSQL, never Core's. Unset, state is kept in memory, which is for one worker and for tests. |
| `CONFIG` | the YAML files and directories, separated by commas |
| `HTTP_ADDR` | where `/healthz`, `/metrics` and `/status` listen (default `127.0.0.1:9090`) |
| `CORE_BASE_URL_ALLOWLIST` | the Core origins or host patterns an agent may point at |
| `CORE_BASE_URL` | the Core the hosted agents connect to: those people connect themselves, kept in the database (`DATABASE_URL`) |
| `SECRETS_DIR` | where `secret://` references are looked for |
| `PRICES` | the price table, instead of the runtime's `prices_ref` |
| `KMS_KEY_ID` | the key that seals the secrets kept in the database: `local:<dir>/<name>`, a 32-byte key in that file |
| `API_ADDR`, `API_AUDIENCE` | where the JSON API for the front end listens, apart from `HTTP_ADDR` (unset, there is none), and the audience Core's assertions name for this runtime, as Core's `RUNTIME_AUDIENCES` lists it ([`docs/design.md`](docs/design.md) §11.4) |
| `CORE_ASSERTION_KEY`, `ADMIN_ACTOR_IDS`, `API_TRUSTED_PROXIES` | Core's assertion key, pinned; the runtime's administrators among Core's; the proxies in front of the API |
| `EGRESS_PROXY` | a proxy for every outbound call |
| `LOG_LEVEL`, `LOG_FORMAT` | `info` and `json` by default |
| `LOG_REDACT_EXTRA` | regular expressions redacted from logs, beside the built-in token and key shapes |
| `WORKER_ID`, `SHUTDOWN_GRACE` | this process's name in leases (default hostname-pid); the grace on `SIGTERM` (default `15s`) |

`aishie-runtime help` lists them as the binary reads them.

## Trying it

```sh
make build
CONFIG=examples/runtime.yaml,examples/agents bin/aishie-runtime check
```

To run an agent for real, start from one of the examples:

1. Issue the agent a token in Core, and seat it in a course (handout §2.6 and §2.7).
2. Put the token and a model key where its `token_ref` and `key_ref` point.
3. Point `core.base_url` at your Core.

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
  set.

[`CONTRIBUTING.md`](CONTRIBUTING.md) covers how changes, migrations and
releases are made.

## Deploying

Every green commit on `main` is published to
`ghcr.io/aishie-education/aishie-agent-runtime` and deployed to staging.
Tags `v*` are releases, and production is deployed by hand from the Deploy
workflow. The image serves no port to the internet: it connects out, to
Core and the model providers. [`docs/deploying.md`](docs/deploying.md) says
how a server is set up, how the workflow is connected, and how to roll
back.
