# Deploying the AIShie Agent Runtime

One server per environment, staging first, production when staging has
earned it. Most likely it is the server Core already runs on, set up with
Core's `deploy/setup-server.sh` (AIShiteru-Core's `docs/deploying.md`): the
runtime adds one container and one database there, and opens no port.

The runtime is the `aishie-runtime` container, on the host's network. It
serves nothing to the internet. It connects out, to Core and to the model
providers, and listens only on `127.0.0.1:9090`, for `/healthz`, `/status`
and `/metrics`. Its state (leases, answers written ahead, cursors, memory,
the ledger) is in a PostgreSQL database of its own, `aishie_runtime`, never
Core's. On the server:

| Where | What |
| --- | --- |
| `/etc/aishie-runtime/runtime.env` | the settings, `DATABASE_URL` among them |
| `/etc/aishie-runtime/agents/` | the agents' configuration, mounted read-only at `/config` |
| `/etc/aishie-runtime/secrets/` | the secrets it refers to, mounted read-only at `/secrets` |
| `/etc/aishie-runtime/secrets/kek/` | the key that seals the secrets kept in the database, and older ones ([below](#the-key-that-seals-secrets)) |
| `/var/backups/aishie-runtime/` | the database's backups |
| `/var/log/aishie-runtime-deploy.log` | every deploy, from what to what |

The scripts in [`deploy/`](../deploy) do the work. They are this
repository's, not Core's, though Core has scripts of the same kind:

- `setup-server.sh` sets a server up. Run again, it installs newer copies of
  the other two scripts and leaves everything else as it is.
- `aishie-runtime-deploy` puts an image on the server. It is used for the
  first start, for every upgrade and for a rollback, by hand or by the Deploy
  workflow.
- `aishie-runtime` runs a one-off command with the image that is running,
  and what the running container sees: `aishie-runtime check`,
  `aishie-runtime migrate version`.

## The server

- Beside Core: nothing more than Core's server has. The runtime uses little
  of it: memory for its pollers, and a database far smaller than Core's.
- On its own: Ubuntu 24.04 or later, 2 CPUs, 2 GB of memory and 20 GB of
  disk, to start with. The database holds who asked what and when (ids, not
  what they wrote) and what each answer cost, so pick a provider and a region
  your institution allows for that.
- Port 22 open, for the Deploy workflow, from GitHub's runners: so, to the
  internet. The only key this repository's workflow uses there can run
  `aishie-runtime-deploy` and nothing else.
- Log in to it with a key, not a password: `setup-server.sh` warns when SSH
  still takes passwords, and Core's `docs/deploying.md` says how to turn
  them off.
- Out: HTTPS to Core, to each provider the agents use, and to `ghcr.io`.
  Behind an egress proxy, set `EGRESS_PROXY` (below).

## Setting a server up

1. Copy this repository's `deploy/` to the server, and run the set-up as
   root, with the server's name, its environment, and the Core the agents
   connect to. On Core's server the Core defaults to Core's own `PUBLIC_URL`.

   ```
   scp -r deploy you@lms-staging.example.edu:aishie-deploy
   ssh you@lms-staging.example.edu
   sudo -i
   sh ~you/aishie-deploy/setup-server.sh lms-staging.example.edu staging https://lms-staging.example.edu
   ```

   It installs Docker and PostgreSQL where they are missing. It creates the
   database `aishie_runtime` and its role, and the env file with a generated
   database password, `HTTP_ADDR=127.0.0.1:9090`,
   `CORE_BASE_URL_ALLOWLIST` and `CORE_BASE_URL` set to that Core and
   `LOG_FORMAT=json`. It
   creates the directories for the agents' configuration and secrets, the
   key that seals the secrets kept in the database, with `KMS_KEY_ID` in the
   env file, the backup directory and a nightly backup, and installs the two
   scripts. Last, it creates the SSH user `aishie-deploy` for this
   repository's Deploy workflow.

   If it stops, fix what it names and run it again. A server that has just
   booted may still be updating itself, and the script waits for that.

2. Let the server pull the image. The package is private, and GitHub's
   registry takes only a personal access token (classic), with
   `read:packages`. Docker keeps one login per registry: on Core's server,
   the account Core's image is pulled with is the one this image is pulled
   with too. Give that account read access to this package (the package's
   settings, Manage access), or log in again, as root, with an account that
   can read both. Core's `docs/deploying.md` says how to make that account
   and its token.

   ```
   docker login ghcr.io -u <that account's user name>
   ```

3. Configure the agents ([below](#the-agents-configuration-and-secrets)), and
   check the configuration with the image you are about to start. This can
   wait: with no agent configured, the runtime starts, is healthy and does
   nothing, and says so in its log at every start and reload, so the first
   deploy can come before the first agent. Every
   green push to `main` publishes
   `ghcr.io/aishie-education/aishie-agent-runtime:sha-<commit>`: the CI run's
   `publish / image` job names it, and so does the package's page. A release
   publishes `:X.Y.Z`. Production takes only releases.

   ```
   AISHIE_RUNTIME_IMAGE=ghcr.io/aishie-education/aishie-agent-runtime:sha-de4f548 aishie-runtime check --live
   ```

4. Start it, and see that it answers:

   ```
   aishie-runtime-deploy ghcr.io/aishie-education/aishie-agent-runtime:sha-de4f548
   curl -s 127.0.0.1:9090/healthz
   curl -s 127.0.0.1:9090/status
   ```

5. The day after, check that the nightly backup ran:
   `ls -l /var/backups/aishie-runtime/daily-*`.

## The agents' configuration and secrets

The agents are YAML files in `/etc/aishie-runtime/agents`: one document per
agent (`agent:` and its `courses:`), and at most one `runtime:` document for
the runtime's own settings, such as the tenants and the price table
(`examples/` in this repository, and in each release's archive). Files
ending in `.yaml` or `.yml` are read, in name order; subdirectories are not.
A relative path in a `*_ref`, such as `system_ref: prompts/tutor.md` or
`prices_ref: prices.yaml`, is relative to the file's own directory, so a
prompt or the price table can sit beside the agents.

Secrets are never written in the YAML (`check` refuses what looks like a
token). A reference says where each one is:

- `secret://agents/cs101-tutor/core_token` is the file
  `/etc/aishie-runtime/secrets/agents/cs101-tutor/core_token`, one value per
  file (a trailing newline is dropped). Where there is no such file, it is
  the variable `AISHIE_SECRET_AGENTS_CS101_TUTOR_CORE_TOKEN` in the env file.
- `env://NAME` is the variable `NAME` in the env file.

Both directories are root's, and the runtime reads them through their group,
65532, the image's user: files `640`, directories `750`. The container
cannot write to either. To add an agent and its Core token (issued as
Core's `docs/deploying.md` says, An agent's token):

```
install -g 65532 -m 640 cs101-tutor.yaml /etc/aishie-runtime/agents/
install -D -g 65532 -m 640 /dev/stdin /etc/aishie-runtime/secrets/agents/cs101-tutor/core_token
    (paste the token, Enter, Ctrl-D)
aishie-runtime check --live
```

`check` loads and validates the configuration as `run` will; `--live` also
resolves the secrets, connects each agent to Core, shows its seats and what
its model is offered, and tries each model key with one call. A
`core.base_url` must be within `CORE_BASE_URL_ALLOWLIST`.

To apply a change to the agents, check it, then either tell the runtime to
read its configuration again, or deploy the image that is running again,
which checks it first and changes nothing if the new version refuses it:

```
aishie-runtime check && docker kill -s HUP aishie-runtime
aishie-runtime-deploy "$(docker inspect -f '{{.Config.Image}}' aishie-runtime)"
```

## Hosted agents

Besides the agents in `/etc/aishie-runtime/agents`, the runtime runs the
agents people connect to it themselves, from AIShiteru-Frontend
(`docs/design.md` §11): the registry, kept in the runtime's database with
their tokens and keys sealed. It is on whenever `DATABASE_URL` is set; the
runtime puts a change to it in force at once, and `check` lists the hosted
agents it would run, and those it would not, with why. `/status` marks them
`hosted`. They connect to `CORE_BASE_URL`, which must be a Core that names
an agent's owner (`me.get`'s `owner_actor_id`, since Core's C1): a hosted
agent runs only while Core names as its owner the person who connected it,
and is stopped otherwise, in state `owner_changed`, or in state `error` on
an older Core, which cannot say. `check --live` checks the owner as `run`
does, and tries a hosted agent's model as `run` calls it (below). A hosted
agent whose id or Core actor is a YAML agent's does not run: the
operator's configuration wins.

## The API for the front end

AIShiteru-Frontend manages hosted agents through the runtime's JSON API
(`docs/design.md` §11.4), served on `API_ADDR` under `/runtime/api/v1/`
and nothing else. The front end reaches it on Core's own origin, through
Caddy, which must strip the `Cookie` header: the API takes Core's
assertions and never a session cookie. In the site block of Core's
origin, before the catch-all `handle`:

```
@runtime path /runtime/api/*
handle @runtime {
	request_header -Cookie
	reverse_proxy 127.0.0.1:9091
}
```

Core makes the assertions: its env file needs `RUNTIME_AUDIENCES` naming
this runtime (`https://lms.example.edu/runtime`, the same bytes as
`API_AUDIENCE`), and `PUBLIC_URL`, which must be `CORE_BASE_URL`. Then, in
`/etc/aishie-runtime/runtime.env`:

```
API_ADDR=127.0.0.1:9091
API_AUDIENCE=https://lms.example.edu/runtime
API_TRUSTED_PROXIES=127.0.0.1/32
```

and deploy the running image again. `curl -s 127.0.0.1:9091/runtime/api/v1/info`
answers with the audience, and `features` says the API connects agents by
their tokens and takes their owners' own keys; `/status` is not there, and
on `HTTP_ADDR` it refuses any request a proxy forwarded, so pointing Caddy
at `9090` by mistake exposes nothing. In the compose stack (aishie-deploy),
the stack sets all of this itself.

Through the API, a person connects an agent of theirs by a token Core
issued it, chooses its model and gives their own key for it, tries a key,
pauses and resumes the agent, gives it a new token and deletes it. What
the runtime does with Core on their behalf is with the agent's own token:
it asks Core what the token is, and revokes the token a new one replaces,
and an agent's token when the agent is deleted, a new token given while it
is being deleted among them. An agent suspended in Core cannot revoke its
tokens; its owner then revokes them in AIShie, as the front end says. Every change, and every refusal, is in the audit
(`docs/design.md` §11.4), with hints of tokens and keys, never the values.

A hosted agent's model is called only at the providers' own endpoints,
which the runtime makes from the provider its owner chose: no one gives it
a URL. The runtime also refuses, when it dials, any address that is not on
the public internet (loopback, private, link-local and the cloud metadata
address, carrier-grade NAT, the IPv6 forms that hold an IPv4 address, and
the rest of the reserved ranges, whatever DNS says), and follows no
redirect; `check --live` tries a hosted agent's model the same way. Behind
`EGRESS_PROXY` it dials only the proxy, which then resolves and connects:
the proxy must refuse those addresses itself, or a hosted agent's calls
are only as closed as the proxy is.

## The key that seals secrets

The tokens and keys of hosted agents, which people give the runtime rather
than an operator writing them in files, are kept in its database, sealed
(`docs/design.md` §11.1): each under a data key of its own, which the key
in `/etc/aishie-runtime/secrets/kek/` wraps. The env file names it:
`KMS_KEY_ID=local:/secrets/kek/v1`, the path the container sees.

`setup-server.sh` makes `v1` when the directory holds no key: 32 random
bytes, base64, `root:65532`, mode `640`, in a directory of mode `750`. The
directory is a keyring: the file `KMS_KEY_ID` names seals new secrets, and
every other file in it still opens what it sealed. Files whose names begin
with `.` are passed over; anything else must be a key, or the runtime does
not start. No `secret://` or `file://` reference may read the directory.

Keep a copy of the directory, encrypted, somewhere other than where the
database's backups go: a backup and the key together are every secret, and
without the key the secrets in a backup are lost. To see that every secret
opens with the keys there:

```
aishie-runtime keys check
```

To replace the key (after someone who could read it leaves, or on a
schedule): add a new one beside the old, point `KMS_KEY_ID` at it, deploy
the running image again so that the new setting is read, rewrap every
secret under it, check, and only then remove the old one.

```
(umask 077 && openssl rand -base64 32 > /etc/aishie-runtime/secrets/kek/.v2.new)
chown root:65532 /etc/aishie-runtime/secrets/kek/.v2.new && chmod 640 /etc/aishie-runtime/secrets/kek/.v2.new
mv /etc/aishie-runtime/secrets/kek/.v2.new /etc/aishie-runtime/secrets/kek/v2
sed -i 's|^KMS_KEY_ID=.*|KMS_KEY_ID=local:/secrets/kek/v2|' /etc/aishie-runtime/runtime.env
aishie-runtime-deploy "$(docker inspect -f '{{.Config.Image}}' aishie-runtime)"
aishie-runtime keys rewrap
aishie-runtime keys check      # every secret opens, and v2 wraps them all
rm /etc/aishie-runtime/secrets/kek/v1
```

Keep the old key's copy until the database's backups older than the rewrap
have rotated out (a week of nightly ones, and the last ten deploys'): their
secrets are still wrapped by it.

## The env file

`/etc/aishie-runtime/runtime.env` is one `NAME=value` per line: no quotes,
no `export`, and no comment after a value. Docker takes quotes and comments
as part of the value. A change to it takes a deploy of the image that is
running (above): a restart does not read the file again.

| Setting | |
| --- | --- |
| `DATABASE_URL` | the runtime's own database. Never Core's: `aishie-runtime-deploy` refuses the one Core's env file names, and backs up only a database on this server. |
| `HTTP_ADDR` | where `/healthz`, `/status` and `/metrics` are served: `127.0.0.1:9090`. Keep it on localhost. |
| `CORE_BASE_URL_ALLOWLIST` | the Core installations an agent may point at, comma-separated: origins (`https://lms.example.edu`) or host patterns (`*.example.edu`). |
| `CORE_BASE_URL` | the Core that hosted agents, those people connect rather than an operator writing YAML, connect to: `https://lms.example.edu`, within `CORE_BASE_URL_ALLOWLIST`. `setup-server.sh` sets it to the Core it was given. Unset, no hosted agent runs, and each one's state says so. |
| `LOG_FORMAT`, `LOG_LEVEL` | `json` (the default) or `text`; `info` by default. |
| `LOG_REDACT_EXTRA` | comma-separated regular expressions removed from every log line, beside the tokens and keys the runtime always removes. |
| `EGRESS_PROXY` | the proxy for every call out (Core, the providers, Core's file downloads); without it, the usual `HTTPS_PROXY`. It must refuse the addresses the runtime refuses hosted agents' models ([above](#the-api-for-the-front-end)): the runtime can check only the proxy's. |
| `SHUTDOWN_GRACE` | how long the runtime lets answers in flight finish on SIGTERM (`15s`). `aishie-runtime-deploy` gives Docker that and 15 seconds more to stop it. |
| `WORKER_ID`, `PRICES` | this worker's name in the leases (the host's name and the process id), and a price table's path when the `runtime:` document names none. |
| `KMS_KEY_ID` | the key that seals the secrets kept in the database: `local:/secrets/kek/v1` ([above](#the-key-that-seals-secrets)). `setup-server.sh` adds it. Unset, no sealed secret opens. |
| `API_ADDR` | where the JSON API for the front end listens, apart from `HTTP_ADDR`: `127.0.0.1:9091` ([above](#the-api-for-the-front-end)). Unset, there is no API. With it, `API_AUDIENCE`, `CORE_BASE_URL`, `DATABASE_URL` and `KMS_KEY_ID` are required, and the runtime does not start without them. |
| `API_AUDIENCE` | the audience Core's assertions name for this runtime, exactly as Core's `RUNTIME_AUDIENCES` lists it, byte for byte: `https://lms.example.edu/runtime`. |
| `CORE_ASSERTION_KEY` | Core's assertion key, pinned: its Ed25519 public key as the `x` of Core's `/v1/auth/keys`. Unset (the usual), the runtime fetches the keys from `CORE_BASE_URL`. |
| `ADMIN_ACTOR_IDS` | Core actor ids, comma-separated: the runtime's administrators are Core's `root` and `admin` accounts among them. Unset, all of Core's. |
| `API_TRUSTED_PROXIES` | the addresses or CIDRs of the proxy in front of the API (`127.0.0.1/32` when Caddy runs on this server), whose `X-Forwarded-For` the audit and the per-address limits believe. Unset, every request seems to come from the proxy, which then shares one allowance. |

`CONFIG` and `SECRETS_DIR` are set by `aishie-runtime-deploy` to the two
mounts, whatever the file says. There is no `OIDC_*`: people sign in to
Core, which vouches for them to the runtime (`docs/design.md` §11.4).
`aishie-runtime help` lists every setting the image in hand reads.

## Connecting the Deploy workflow

`setup-server.sh` ends by printing three settings. Add them in this
repository's Settings → Secrets and variables → Actions (not Core's):

| Kind | Name | Value |
| --- | --- | --- |
| Variable | `DEPLOY_TARGET_STAGING` | `aishie-deploy@lms-staging.example.edu` |
| Variable | `DEPLOY_KNOWN_HOSTS_STAGING` | the server's host key line, as printed |
| Secret | `DEPLOY_SSH_KEY_STAGING` | the whole of `/root/aishie-runtime-deploy-key` |

Then delete `/root/aishie-runtime-deploy-key` from the server. The server
keeps only the public half, in `~aishie-deploy/.ssh/authorized_keys`.

For production, the names end in `_PRODUCTION`. SSH on a port other than 22
is `ssh://aishie-deploy@host:2222` in the target and
`[host]:2222 ssh-ed25519 …` in the host key line.

The key is this repository's alone, and logs in as a user of its own,
`aishie-deploy`, not Core's `deploy`: each repository can deploy only its own
image, and Core's `setup-server.sh` writes `deploy`'s keys whole, so a second
key there would not last.

From then on, every green push to `main` deploys to staging, and a
pre-release tag (`v1.2.3-rc.1`) does too. To try the connection without a
push, go to Actions → Deploy → Run workflow, from `main`, with environment
`staging` and image `ghcr.io/aishie-education/aishie-agent-runtime:edge`. That
is also the way to deploy staging again: re-running the deploy of an older
push to `main` fails once `main` has moved on. (Re-running a pre-release's
deploy, or a Deploy run by hand, still deploys the image it had.) Production
is deployed only by running Deploy by hand, from a release's tag
([CONTRIBUTING.md](../CONTRIBUTING.md#releasing)).

The key only runs `aishie-runtime-deploy`, but that script deploys any image
of this repository. Anyone with write access to the repository can run a
workflow that reads the secret, or copy the key out. They can also push an
image of their own under this repository's name and deploy it, and the
runtime holds the agents' Core tokens and the providers' keys. On GitHub
Free, nothing narrows that down to a branch or to people: write access is
access to everything the runtime can reach. When someone loses write
access, replace the key, delete any package versions they pushed, and
replace the agents' tokens and keys.

To replace the key: on the server, delete `~aishie-deploy/.ssh/authorized_keys`
and any `/root/aishie-runtime-deploy-key*` left, run `setup-server.sh` again
as in step 1, with the server's name and its environment, and put the new key
it prints into the secret it names.

## Deploying and rolling back

`aishie-runtime-deploy IMAGE` takes an image of this repository, by tag or by
digest, and nothing else. One deploy runs at a time on the server. In order:

1. It pulls the image and reads its version.
2. `aishie-runtime check` with the new image, on the configuration as it is
   on the server. If the new version refuses it, nothing changes.
3. It backs up the runtime's database (`deploy-*.dump`, the last ten kept).
4. `aishie-runtime migrate up` with the new image. `run` never migrates on
   its own, and refuses a schema older than it. A schema a newer release
   migrated is left as it is.
5. It stops the running container, letting it finish the answers in flight
   (`SHUTDOWN_GRACE`), and starts one of the new image, as the image's
   nonroot user, with the env file and the two read-only mounts.
6. It waits for `/healthz` to answer status `ok` with the new version and
   commit. If it never does, it prints the new container's last log lines,
   starts the version that was running before again, and waits for that one
   in turn.

Up to step 5, the version that was running goes on running. An answer cut
short by the stop is taken up again by the version that takes over: one
already on its way to Core is sent again under the same key, which Core
takes only once.

**Rolling back** to the release before is a deploy of its image, by hand or
by running Deploy from the newest release's tag with the older image. The
new schema is left as it is, and the release before works with it. Never
run `migrate down`: it deletes all of the runtime's state, the ledger
included. Going back further than one release means restoring a backup.

```
aishie-runtime-deploy ghcr.io/aishie-education/aishie-agent-runtime:1.2.2
```

## Day to day

Run all of these as root on the server. `9090` stays on localhost:
`/status` names agents, courses and members, and nothing asks who is
asking but its address (it answers 403 to any client not on this
machine's loopback, should `HTTP_ADDR` listen wider).

- **Is it up, and which version:** `curl -s 127.0.0.1:9090/healthz` answers
  `{"status":"ok","version":…,"commit":…}` while the runtime runs, and 503
  when its database cannot be reached. `aishie-runtime migrate version` shows
  the schema; `/var/log/aishie-runtime-deploy.log` lists every deploy.
- **The agents:** `curl -s 127.0.0.1:9090/status` is each agent's state
  (running, paused, unauthorized, …), its seats and what holds any back, the
  proposals waiting, the answers and spend today, and the catalogue's hash.
  An agent `unauthorized` has a Core token that no longer works: issue a new
  one, put it in its secret file, and reload; a hosted agent's owner
  connects it again with a new token instead. A hosted agent
  `owner_changed` is one whose owner in Core is no longer the person who
  connected it here (Core names someone else, or no one): it does not run
  until its owner in Core connects it again. A hosted agent in state
  `error` that says Core does not say who owns an agent is on a Core from
  before owners were named (C1): upgrade Core, then reload the runtime.
- **Metrics:** `curl -s 127.0.0.1:9090/metrics`, in Prometheus's format, for
  a Prometheus on the same machine, or through an SSH tunnel. The one to
  watch is `presence_gap_seconds`: above 60, Core shows the agents as away.
  A hosted agent's model calls are counted under the model's name as the
  price table gives it, or `other` when the table does not price it: the
  model its owner typed is never a label.
- **Logs:** `docker logs -f aishie-runtime`. One JSON line per event: ids,
  counts, outcomes and timings, never what anyone wrote, and no token or
  key. Docker keeps the last 100 MB.
- **Checking the agents' connections:** `aishie-runtime check --live`.
- **Upgrading Core:** the runtime checks Core's tool catalogue at start and
  refuses one whose tools it relies on have gone or changed kind. Each
  runtime release is tested against the Core its `.github/core-image` pins;
  upgrade Core in staging first, and look at `/status` after.
- **Updating the scripts:** when `deploy/` changes, copy it to the server
  again and run `setup-server.sh` as in step 1. It installs the new scripts
  and leaves the rest.
- **Disk:** after each deploy, `aishie-runtime-deploy` removes this
  project's images that no container uses, and never Core's. A rollback pulls
  its image again.

## When something goes wrong

- **A deploy failed before the new version started.** For example, the pull
  was refused, the configuration did not pass `check`, or a migration
  failed. The old version is still running. The last lines of
  `aishie-runtime-deploy`'s output, and of the workflow's log, name the step
  and its error.
- **A migration failed.** `aishie-runtime-deploy` stops at `migrate up` with
  the migration's error, and the old version keeps running. The schema is
  marked dirty at N, the migration that failed, and no version of the
  runtime starts on it until that is put right. Each migration runs in one
  transaction, so a failed one has usually left nothing behind. Fix the
  cause the error names, record the migration before it as the last one
  applied, and deploy again:

  ```
  runuser -u postgres -- psql -d aishie_runtime -c 'UPDATE schema_migrations SET version = <N - 1>, dirty = false'
  aishie-runtime-deploy <the same image>
  ```

  If you are not sure what the failed migration left, restore the backup
  instead.
- **The new version did not report healthy.** `aishie-runtime-deploy`
  printed the new container's last log lines. If another version was running
  before, it is started again, with the env file and the configuration as
  they are now, and the last line says whether it came up. If it did not,
  or if the same image was deployed again (most likely after a change to the
  env file), nothing healthy is running: fix what the log names and deploy
  again.
- **Restoring a backup.** `aishie-runtime-deploy` takes one before every
  deploy (`/var/backups/aishie-runtime/deploy-*.dump`, the last ten), and
  cron takes one every night (`daily-1.dump` to `daily-7.dump`). What was
  written after the backup is lost: answers already posted to Core stay
  there, and the runtime, missing their attempts, may find their keys taken
  and move on to the next attempt number. Deploy the image that was running
  when the backup was taken, which `/var/log/aishie-runtime-deploy.log` tells
  you:

  ```
  docker stop aishie-runtime
  runuser -u postgres -- dropdb aishie_runtime
  runuser -u postgres -- createdb -O aishie_runtime aishie_runtime
  runuser -u postgres -- pg_restore --exit-on-error -d aishie_runtime /var/backups/aishie-runtime/<file>.dump
  aishie-runtime-deploy <the image from then>
  ```

## Backups off the server

The backups above sit on the same disk as the database. Copy these somewhere
else regularly:

- `/var/backups/aishie-runtime/`, the database;
- `/etc/aishie-runtime/`, the env file, the agents' configuration and their
  secrets. It holds every agent's Core token and the providers' keys: keep
  the copy encrypted.
- `/etc/aishie-runtime/secrets/kek/`, the keyring, which is in the copy
  above: keep that copy apart from the database's. The database's backups
  hold the hosted agents' secrets sealed, and the keyring opens them; the
  two in one place are every secret in the clear.

## More than one worker

Workers share agents by lease, in the database, so more than one can run:
on other servers, each with the same configuration and secrets, and the
same `DATABASE_URL`, which then cannot be `127.0.0.1`. `aishie-runtime-deploy`
and the Deploy workflow handle one server per environment, with its database
on that server; a database elsewhere is backed up by whoever runs it, and
the script refuses it.
