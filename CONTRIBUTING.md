# Contributing

## The loop

```
make ci        # what CI runs: lint (gofmt, go mod tidy, the workflows, go vet, golangci-lint, shellcheck),
               # the scripts' tests, the Go tests with the store on Postgres, the end to end against Core
make live      # the adapters against the real providers whose keys are set
make live-core # Core's client and a seat's toolset against a throwaway Core, when the pin moves
make docker    # the image, as CI builds it; never pushed
```

`make test` wants a PostgreSQL whose role can create databases:
`TEST_DATABASE_URL` names its maintenance database (`postgres:///postgres`,
over the local socket, by default). `make e2e` runs the Core pinned in
`.github/core-image` in Docker, pulled from ghcr.io with no login, as Core's
image is public; without Docker, give it a binary of Core: `CORE_BIN=../AIShie-Core/bin/aishie-core make e2e`.
The scripts run it by whatever name it has, so a Core from before its
rename, whose binary is `aishiterud`, runs as well.

Work on a branch and open a pull request against `main`. `main` is protected:
it takes changes by pull request only, with `lint`, `test (pg 13)`,
`test (pg 18)`, `end to end, against the pinned Core` and `build` green. CI
also runs `vuln`, and all of it again every week.

## Where things go

- A new **adapter**, for a model API the runtime does not speak yet, is a
  package of its own under `internal/llm/` (design §3): its translation both
  ways over `internal/llm/httpx`, not the provider's SDK, with golden tests in
  its `testdata/` for every stop reason and usage field, and a `Live` test
  that `make live` runs when the provider's key is set. It is built in
  `internal/llm/providers`, its defaults (capabilities, schema dialect) are in
  `llm.Defaults`, and its name is one `model.adapter` takes in
  `internal/config`. Give the nightly `live.yml` its key as a repository
  secret and its model as a variable. A provider that speaks an API the
  runtime already has (most speak OpenAI's Chat Completions) is not a new
  adapter: it is a row in `llm.DetectProvider` and `llm.Defaults`.
- A new **tool gate**: `GET /v1/tools` does not say which permissions each
  tool needs, so the gates are kept by hand, in `toolset.Gates` for reads
  and `toolset.WriteGates` for writes (Core's `docs/agent-runtime.md` §4,
  `docs/design.md` §4). A tool Core adds is offered to no model until it has
  a gate there, with the permissions Core's own declaration of the tool
  checks, and a test; a write goes only to a conversation its agent's owner
  opened. `toolset.BuiltinDeny` keeps out the tools no model may call
  whatever the configuration and the seat's perms allow, each with its
  reason: a new write Core adds is either gated or denied there, which a
  test holds. At start, the runtime checks every gated tool against Core's
  catalogue, and refuses one that has gone or changed kind.
- A **store migration** is the next `NNNN_name.up.sql` and
  `NNNN_name.down.sql` in `internal/store/pgstore/migrations`, both, each one
  transaction ([Migrations](#migrations)). A change to what the store keeps
  goes into `internal/store`'s interfaces, `memstore` and `pgstore` alike,
  with a test in `storetest`, which runs against both.
- A new **call to Core** is a typed call in `internal/core`, the same tool in
  the fake Core (`internal/fakecore`), and a fixture recorded from the real
  one, which the fake is held to.
- A new **setting** is read in `internal/config` (`FromEnv`, with its line in
  `EnvHelp`), and, if a server needs it, in `docs/deploying.md`.
- **Logs hold ids, counts, codes and timings, never what anyone wrote**, and
  every line goes through `redact`.

## Tests and time

A test waits on what it tests, never on how fast the machine is: a busy
runner, or a laptop doing other things, stretches every sleep and timer,
and a test that passed alone then fails.

- How long code decides to wait is read off what it decided: a timer or
  clock the test holds (the OCR and Office services' `after` and `now`,
  the worker's `Options.Now`), or the deadline it gives a call; not off
  how long the call took.
- An event is waited for (`eventually`, a channel the code closes, a
  counter in a fake), never slept for. A step that must come before
  another is made to: a scripted model that waits on a channel the fake
  Core closes, not one that waits long enough.
- "Nothing more happens" is held over events that keep happening (lease
  ticks a store counts, calls another agent begins), and counts what was
  begun after, not what landed after (`countCalls` in the worker's
  tests): a call cancelled in flight is logged by the fake Core when it
  ends. Likewise what is counted when it ends is read once it has ended,
  not once the test has stopped it.
- Where a time bound is itself the point, it is generous (seconds, for a
  program the sandbox runs below the runtime's priority), tied to what it
  bounds (an agent whose lease renewal hangs stops before the lease would
  lapse, on a lease of seconds), or reckoned from what was measured.
- A fake reached over the loopback is a server like any other: a busy
  machine is slow to accept its connections, and the kernel resets those
  past its listener's backlog. A test that makes many calls at once keeps
  its connections between calls, and makes again a connection reset as
  it is made, which carried no call (`loadTransport` in the worker's
  load test); a call that failed once under way still fails.
- No test is skipped or retried for being slow.

Try a test that waits under load before calling it done, with the race
detector and many runs, beside a few busy processes that end with it,
in zsh or bash; `go test`'s `-timeout` bounds it, interrupted or not:

```
(
  pids=()
  trap 'kill "${pids[@]}" 2>/dev/null' EXIT
  trap 'exit 130' INT TERM
  for _ in $(seq 16); do yes > /dev/null & pids+=($!); done
  go test -race -count=50 -timeout 10m -run 'TestName$' ./internal/worker
)
```

## Migrations

`NNNN_name.up.sql` and `NNNN_name.down.sql`, both required, each one
transaction. `run` never migrates on its own, and refuses a schema older
than it; `aishie-runtime-deploy` runs `migrate up` with the new image while
the old one keeps running, and a rollback runs the release before on the
schema as it is. So a migration must leave the previous release working: it
is additive. Add a column in one release, stop using the old one in the
next, drop it in the one after.

A migration that has reached `main` has run on edge: never delete,
renumber or rewrite it; undo it with a new one. `migrate up` leaves a schema
that is ahead of the binary as it is, since that is what a rollback looks
like, so a revert that takes a migration out leaves edge at a version
`main` no longer has, and the next migration to take its number is never
applied there. If one must go, run `migrate down` with an image that still
has it before the revert is deployed.

`scripts/release-notes.sh` lists the migrations new in a release for its
notes and for the Deploy workflow's summary.

## Releasing

A push to `main` goes out by itself once CI passes: its image is pushed to
GHCR as `:sha-<commit>` and `:edge`, which edge servers pull within five
minutes ([AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy)).
When pushes come faster than they are published, one that a newer push
overtakes while it waits is not published. A release is made by a tag, from
`main`. Run `make live-core` on the commit to be tagged first, and
`make live` where you have the providers' keys: the release re-runs CI,
which runs neither (docs/design.md §10).

```
git switch main && git pull
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`release.yml` runs the whole of CI again on the tagged commit, the end to end
against the pinned Core included, then publishes binaries (Linux and macOS,
amd64 and arm64, with the docs and the example configuration) with checksums
to the release page, and a multi-architecture image to
`ghcr.io/aishie-education/aishie-agent-runtime`. The release notes list the
store's migrations new in the release; for a stable release, new since the
last stable one, pre-releases included. A tag with a hyphen
(`v0.1.0-rc.1`) is a pre-release: it leaves `:latest` and `:stable` alone.
A stable release moves `:stable` to itself when it is the highest stable
release.

A stable release goes to a school's site when its operator names it in the
server's `/etc/aishie/aishie.env` (AIShie-Deploy's README, Upgrading
stable): that is the decision to deploy and to migrate. Going back is
pinning the release before (its README, Rolling back): `migrate up` leaves
a schema a newer release migrated as it is, and a migration keeps the
release before it working. A release further back may need what a later
migration has dropped.

To a server of Core's own that the runtime joins (the older way,
[docs/deploying.md](docs/deploying.md)), somebody runs **Deploy** for it
instead: Actions → Deploy → Run workflow, use the workflow from the
release's tag, and give the environment `stable` and the image the release
run's summary names (`ghcr.io/aishie-education/aishie-agent-runtime:1.2.3`).
For stable, Deploy takes nothing else: run from a branch or a pre-release's
tag, or given an image that is not a stable release's, it stops before it
deploys. To roll back there, run Deploy from the newest release's tag, whose
checks are the current ones, with the image of the release before.

To try the build without publishing anything:

```
goreleaser release --snapshot --clean
```

### One-time settings

Before the first push to `main` after the CD workflows land, in GitHub:

- **Environments** (repository Settings → Environments): `edge` and
  `stable`. Let `edge` take branch `main` and tags `v*`, and `stable` tags
  `v*` only (Deployment branches and tags → Selected branches and tags).
  Create them first: a run that names an environment that does not exist
  creates it, with no rules. The repository is public, so its environments
  take protection rules on GitHub Free: add required reviewers to `stable`.
  A repository set up when they were called `staging` and `production` needs
  `edge` and `stable` made as well, with the same rules
  ([docs/deploying.md](docs/deploying.md#settings-from-before-the-rename)).
- **Packages** (organization Settings → Packages): Default Package Settings
  should keep "Inherit access from source repository". The first publish
  then creates `aishie-agent-runtime` linked to this repository, which its
  workflows can write to. The package must be public (its settings → Danger
  Zone → Change visibility → Public): servers pull it with no login. Do not
  push the image by hand before the first publish: a package pushed from
  outside a workflow is not linked, and the workflow cannot push to it until
  it is given access (package settings, Manage Actions access).
- **Allowed actions** (organization Settings → Actions → General →
  Policies): if the organization allows only selected actions, allow
  `docker/*` and `goreleaser/*` with the rest. Pull requests' CI uses only
  `actions/*`, so a policy that leaves the others out first shows at the
  first publish or release.
- **Variables and secrets**, when they apply. Releases are attested with no
  setting, the repository being public (a private one would need GitHub
  Enterprise Cloud and `ATTESTATIONS` = `true`). For each environment with a
  server of Core's own that the runtime joins (the older way:
  [docs/deploying.md](docs/deploying.md)), the repository variables
  `DEPLOY_TARGET_EDGE` and `DEPLOY_KNOWN_HOSTS_EDGE` and the repository
  secret `DEPLOY_SSH_KEY_EDGE` (`_STABLE` for stable), which
  `deploy/setup-server.sh` prints. They are this repository's, not Core's,
  and not the environment's, as the workflow was written when the
  repository was private and GitHub Free gave it no environment variables
  or secrets.
  For `live.yml`, each provider's key as a secret (`OPENAI_API_KEY`, …) and
  its model as a variable (`OPENAI_MODEL`, …), and Azure's endpoint and
  deployment as variables (`AZURE_OPENAI_BASE_URL`,
  `AZURE_OPENAI_DEPLOYMENT`); a provider without a key is skipped.
- **Minutes and storage**: the repository is public, so its Actions minutes
  on GitHub's standard runners cost nothing, and neither does a public
  package's storage. Every push to `main` runs the whole of CI, the end to
  end against Core included, and a two-architecture image build; `live.yml`
  runs every night. Every green push also leaves a `:sha-*` image, with its
  SBOM and provenance. Nothing deletes old images automatically, since deleting untagged versions
  can break a multi-architecture image; prune them from the package page
  when needed.

## Moving the Core pin

`.github/core-image` names the Core the runtime is checked against, by tag
(Core's commit, or a release) and by digest, which is what is pulled. Two
things were taken from that Core and must match it:

- `internal/core/testdata/catalogue.json`, its tool catalogue: the end to
  end fails when the pinned Core's differs, and the tool gates, the
  sanitiser's tests and the fake Core are built on it;
- the fake Core's fixtures, `internal/fakecore/testdata/fixtures`: envelopes
  recorded from it for every outcome of an answer, which the fake is held
  to.

Move the three together, in one pull request:

1. Write the new image's tag and digest as the last line of
   `.github/core-image`. Core's CI run for a push to its `main` names the
   image (`publish / image`), as does the package's page; a release is
   `:X.Y.Z`. The digest:

   ```
   docker buildx imagetools inspect ghcr.io/aishie-education/aishie-core:sha-<commit> --format '{{json .Manifest}}' | jq -r .digest
   ```

   giving `ghcr.io/aishie-education/aishie-core:sha-<commit>@sha256:<digest>`.
   `scripts/ci-core.sh` refuses a `:sha-` tag whose commit is not the
   digest's.

2. Start that Core on a scratch database, and take the catalogue and the
   fixtures from it:

   ```
   make build
   DATABASE_URL=postgres:///aishie_pin_core scripts/ci-core.sh start
   . "${TMPDIR:-/tmp}/aishie-ci-core/env"
   bin/aishie-runtime catalogue --core "$E2E_CORE_URL" --write internal/core/testdata/catalogue.json
   jq -S --indent 1 . internal/core/testdata/catalogue.json > catalogue.tmp
   mv catalogue.tmp internal/core/testdata/catalogue.json
   cp internal/core/testdata/catalogue.json internal/fakecore/testdata/catalogue.json
   make record-fixtures
   scripts/ci-core.sh stop
   ```

3. Read the diffs. A tool gone from the catalogue, or of another kind, or a
   new tool the agents should have, is a change to the tool gates
   ([Where things go](#where-things-go)); a changed schema shows in the
   sanitiser's tests. A changed fixture is a change in Core's behaviour: the
   fake Core must follow it, and `make test` fails until it does.

4. `make ci` and `make live-core` (the live tests against a Core of their
   own, which CI does not run: docs/design.md §10), then push the three
   together. From then on, every pull request's end to end, and every
   release's, runs against the new Core.
