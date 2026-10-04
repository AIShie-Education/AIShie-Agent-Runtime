# Deploying the AIshie Agent Runtime

A site runs the runtime with Core and the web front end, as one stack per
server, from [AIShie-Deploy](https://github.com/AIShie-Education/AIShie-Deploy),
which keeps itself up to date and sets most of what follows itself: that is
the way to run one. This document is the older way, the runtime added to a
server of Core's own and deployed over SSH by this repository's Deploy
workflow. What it says of the runtime's own settings, from [The agents'
configuration and secrets](#the-agents-configuration-and-secrets) to [The
key that seals secrets](#the-key-that-seals-secrets), holds in the stack as
well.

One server per environment: edge first, the test site every green push to
`main` reaches, and stable, the site a school runs on releases, when edge
has earned it. Most likely it is the server Core already runs on, set up with
Core's `deploy/setup-server.sh` (AIShie-Core's `docs/deploying.md`): the
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
  disk, to start with. OCR ([below](#scanned-documents-ocr)) takes one of
  the CPUs while it reads a scan, and some 200 MB of memory; LibreOffice
  ([below](#presentations-and-documents-libreoffice)) one while it converts
  a deck, and some 400 MB. The database
  holds who asked what and when (ids, not what they wrote) and what each
  answer cost, so pick a provider and a region your institution allows for
  that.
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
   scp -r deploy you@lms-test.example.edu:aishie-deploy
   ssh you@lms-test.example.edu
   sudo -i
   sh ~you/aishie-deploy/setup-server.sh lms-test.example.edu edge https://lms-test.example.edu
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

2. Configure the agents ([below](#the-agents-configuration-and-secrets)), and
   check the configuration with the image you are about to start. This can
   wait: with no agent configured, the runtime starts, is healthy and does
   nothing, and says so in its log at every start and reload, so the first
   deploy can come before the first agent. Every
   green push to `main` publishes
   `ghcr.io/aishie-education/aishie-agent-runtime:sha-<commit>`: the CI run's
   `publish / image` job names it, and so does the package's page. A release
   publishes `:X.Y.Z`. Stable takes only releases. The image is public: the
   server pulls it with no login.

   ```
   AISHIE_RUNTIME_IMAGE=ghcr.io/aishie-education/aishie-agent-runtime:sha-de4f548 aishie-runtime check --live
   ```

3. Start it, and see that it answers:

   ```
   aishie-runtime-deploy ghcr.io/aishie-education/aishie-agent-runtime:sha-de4f548
   curl -s 127.0.0.1:9090/healthz
   curl -s 127.0.0.1:9090/status
   ```

4. The day after, check that the nightly backup ran:
   `ls -l /var/backups/aishie-runtime/daily-*`.

## The agents' configuration and secrets

The agents are YAML files in `/etc/aishie-runtime/agents`: one document per
agent (`agent:` and its `courses:`), and at most one `runtime:` document for
the runtime's own settings, such as the tenants and the price table
(`examples/` in this repository, and in each release's archive). Files
ending in `.yaml` or `.yml` are read, in name order; subdirectories are not.
A relative path in a `*_ref`, such as `system_ref: prompts/tutor.md` or
`prices_ref: prices/prices.yaml`, is relative to the file's own directory, so a
prompt or the price table can sit beside the agents, in a subdirectory: a
price table named `.yaml` in the agents' directory itself would be read as
configuration, and refused.

Each agent names its agent in Core by its id, `core.agent_id`: an agent
Core hosts `runtime` (chosen for good when it is made, in Core's My agents
or by an administrator's `actor.register`; an `mcp` agent is its owner's
tools' to reach, and the runtime does not host it). Nobody gives the
runtime an agent's token: the runtime is issued it by the agent's id, with
its own credential in Core ([below](#the-runtimes-own-credential-in-core)),
as it starts the agent, and keeps it sealed in its database (with
`KMS_KEY_ID`; in memory without it, issued again at each start). Two
agents of the configuration may not name one agent in Core: each would be
issued its one token, revoking the other's. A configuration that still
names a token (`core.token_ref`, from before) does not load: replace it
with `core.agent_id`, and delete the token's file.

Secrets are never written in the YAML (`check` refuses what looks like a
token). A reference says where each one is:

- `secret://keys/openai` is the file `/etc/aishie-runtime/secrets/keys/openai`,
  one value per file (a trailing newline is dropped). Where there is no
  such file, it is the variable `AISHIE_SECRET_KEYS_OPENAI` in the env file.
- `env://NAME` is the variable `NAME` in the env file.

Both directories are root's, and the runtime reads them through their group,
65532, the image's user: files `640`, directories `750`. The container
cannot write to either. To add an agent (its id is in Core's My agents, or
`actor.get`):

```
install -g 65532 -m 640 cs101-tutor.yaml /etc/aishie-runtime/agents/
aishie-runtime check --live
```

`check` loads and validates the configuration as `run` will; `--live` also
resolves the secrets, reads each agent in Core with the runtime's own
credential (how Core hosts it, its live seats, whether people may ask it
now), connects with the token the runtime holds for it, if any, to show
its seats and what its model is offered (its reads, with the runtime's
own `course_materials_search`, and `attachment_get` where a
conversation's messages carry files; and the writes its owner's
conversations are offered besides), and tries each model key with one
call. It is issued no token itself: that would revoke the one a
running runtime holds. A `core.base_url` must be within
`CORE_BASE_URL_ALLOWLIST`.

`check` also shows, as `deprecated:`, what an agent's settings hold that
the runtime takes but no longer does as they say, and `run` logs it when it
puts the configuration in force. The runtime closes no conversation: a
question whose attempts are spent waits until the next day. So
`answer.on_attempts_exhausted: close` is done as `skip`, the default, and
`prompt.close_reason_text` is unused; leave both out when next editing the
file.

The notices an agent posts in place of an answer (`prompt.on_refusal_text`,
`on_budget_text`, also posted when every provider failed five times, and
`on_quota_text`) are the runtime's own unless set: in the asker's
language, as `answer_language` fixes it or the question is written
(English, or Chinese in either script), else in English and Traditional
Chinese (`docs/design.md` §5.3, Notices). A text set, in an agent's
settings or in `runtime.defaults`, is posted as it is, whatever the
asker's language: leave them out for notices in the asker's language. A
notice names no sources: the site shows none under it.

An agent's model reads the course as far as its seat's permissions allow.
It may also change the course, as far as they allow (writing a document, a
grade, an assignment: Core decides each change at the seat's level, and
one at `confirm_required` waits for a person's approval), but only in a
conversation its owner opened, and only when its configuration says
`tools: {writes: true}`: a YAML agent's owner is whoever its operator says,
so writes are off until the operator turns them on. Anyone else the agent
answers, a course tutor's students among them, gets reads alone. Each
answer makes at most `budgets.per_answer.max_writes` changes (10), and
`tools.deny` or `tools.allow` narrow which, as they do reads
(`docs/design.md` §4).

The course's members (seating people, pausing or removing them, changing
what they may do) an agent manages only when its seat holds
`member_manage`, which only an instructor gives, and never more than they
hold. Core gives a person's own agent no more of it than the person holds:
an instructor's own agent, hosted or not, may be given it, and manages the
members in its owner's conversations; a student's may not. An agent nobody
owns may be seated with it too (`member.add`), and runs here from YAML with
`tools: {writes: true}`, answering whoever Core lets address it (those who
hold at least what it holds). It never changes its own seat, the seat of
the person it acts for, nor the seat of another agent of theirs, whatever
it is told, and never seats agents; to tell another agent's seat, the
runtime reads each seat a member write names with `member_get`, so such a
seat needs `member_read` as well, or its changes to a seat are refused.
`tools.deny: [member_*]` takes member changes away from it altogether. The roster (`member_list`, `member_get`) is read where the
seat's `member_read` allows, as any read is.

A seat that holds `action_decide` (an instructor's own agent, say) reads
the queues of proposals, and in its owner's conversations recommends
decisions and reviews: Core holds an agent's `action_decide` at
`confirm_required`, so each is a proposal a person confirms, and the
runtime refuses one from a seat at any other level. `tools.deny:
[action_decide, action_review]` takes them away.

A course document's file reaches the model as the runtime reads it: text
as text; a presentation or a document (`.pptx`, `.ppt`, `.odp`, `.docx`,
`.doc`, `.odt`, `.rtf`) as the PDF LibreOffice makes of it where the model
takes files, and as its text otherwise
([below](#presentations-and-documents-libreoffice)); a workbook as its
text for every model; a PDF as a file where the model takes files (and
its provider a PDF of its size), in parts of ten pages when it has more,
and as its text otherwise; an image as a file where the model takes
files. The runtime reads files of at most 50 MB (as large as Core takes an upload), from memory and within
fixed limits. Text too long for one result (32 KB) is given in parts,
which the model asks for one after another; each worker keeps what it
read of a file (at most 32 MiB in all), so that the file is fetched and
read once for all its parts. A
scanned PDF, or one whose fonts do not map to text, and an image reach a
model that takes no files as the text the runtime's OCR recognizes of
them ([below](#scanned-documents-ocr)), marked as OCR's; without OCR, as a
note asking for a version with selectable text. Without LibreOffice, a
PowerPoint, Word or Excel file reaches every model as the runtime's text
of it, and an older Office file (`.doc`, `.ppt`, `.xls`) as a note asking
for `.pptx`, `.docx` or `.xlsx`, or a PDF. Whether a model takes files is
its adapter's default for its provider, which
`model.capabilities.file_input` overrides.

### Scanned documents (OCR)

The image holds tesseract, with the Chinese (simplified and traditional)
and English data Debian packages (the fast models, 2 to 4 MB a language),
and poppler's `pdftoppm`, which renders a PDF's pages. With them the
runtime recognizes the text of a scanned PDF, of one whose fonts do not
map to text, and of an image, for a model that cannot take the file, or
a PDF past what its provider takes; a model that takes the file is still
given it, and nothing is recognized for it. `docs/design.md` §4 (OCR) has
how it works and how it is held in.

- **The image** is Debian 13 slim with those packages, instead of
  distroless: some 240 MB unpacked and 92 MB to pull (for amd64), where it
  was 30 MB and 10 MB; LibreOffice more than doubles that
  ([below](#presentations-and-documents-libreoffice)). It still runs as `65532:65532`, with the same
  entrypoint, and needs nothing new of `aishie-runtime-deploy`.
- **What it costs:** one CPU while a file is read, at a lower priority
  than the runtime's own work, so that answering is not starved. At the
  default 300 dpi, a dense page of Chinese (some 1,300 characters) takes
  about 9 s of CPU, a sparse one 1 to 2 s, a blank one well under 1 s: a
  40-page scan, a few minutes. `OCR_DPI=200` roughly halves that and reads
  small print less well. tesseract takes about 200 MB of memory while it
  reads a page, within the 1 GiB (`OCR_MEMORY_MB`) it may have.
- **How the model sees it:** the first question about a scan starts it in
  the background and waits for it up to 5 s (`OCR_WAIT`), never past half
  the answer's time left. A long scan is not ready by then: the model is
  told it is being read, how far it has come, and to ask again in a
  minute, with the call to make, and it says so to the person or asks
  again. Once read, the text is kept in the database by the file's
  checksum (`ocr_text`, 180 days) and every worker gives it at once, in
  parts as any long text is, marked `extracted_from: "ocr"` with a note
  that it may hold recognition errors. A failure is kept a day, then tried
  again.
- **The knobs** (in the env file, [below](#the-env-file)): `OCR` (`auto`,
  `on` or `off`), `OCR_LANGUAGES`, `OCR_MAX_PAGES` (40), `OCR_DPI` (300),
  `OCR_PAGE_TIMEOUT` (90s), `OCR_TIMEOUT` (15m), `OCR_MEMORY_MB` (1024),
  `OCR_CONCURRENCY` (files at once per worker: 1; 2 on a server with CPUs
  to spare), `OCR_QUEUE` (files waiting: 8) and `OCR_WAIT` (5s).
- **Is it on:** the start's log line says `ocr` and what runs it, or why
  not, and so does `aishie-runtime check`: `ocr: tesseract 5.5.0
  chi_sim+chi_tra+eng 300dpi, 1 at once, 40 pages a file at most`. With
  `OCR=auto` (the default) and the programs missing (a binary run outside
  the image), OCR is off with a warning, and models are told there is no
  OCR here; `OCR=on` refuses to start without them.
- **In the site:** where OCR runs, the runtime's administrators turn it
  off and on, and choose its languages among those installed, from the
  front end ([below](#what-the-sites-administrators-change)); a change is
  in force on every worker within moments, with no restart, and a file
  recognized in other languages is recognized again when next asked.
  The environment stays the ceiling: with `OCR=off`, or the programs
  missing, the site cannot turn it on, and says why. `check` says the
  site's setting when it has one. The image has Chinese (simplified and
  traditional) and English alone; another language is another image.
- **Watching it:** `ocr_jobs_total{kind,outcome}`, `ocr_pages_total`,
  `ocr_job_seconds`, `ocr_page_seconds{step}`, `ocr_jobs_running`,
  `ocr_jobs_waiting` and `ocr_requests_total{result}` in `/metrics`; a
  `busy` result is a file not started, most often because the queue was
  full. One log line a file, with the
  start of its checksum, its outcome, pages and time, never its text.

### Presentations and documents (LibreOffice)

The runtime's text of a deck names its pictures, charts and diagrams and
no more, and a lecture's slides are much made of them: screenshots of
code, formulas, drawings. So the image holds LibreOffice (its Impress,
Writer and Calc, without their windows), which converts a presentation or
a document to PDF, a page a slide, and a model that takes files sees the
slides as they look, their speaker notes beside them as text. For a model
that takes no files, a deck is its text as before, with what the
runtime's OCR reads of each slide that shows a picture or a chart, and an
older or OpenDocument document is the text of its PDF. `.xls` and `.ods`
are converted to `.xlsx` and read as any workbook. `docs/design.md` §4
(Office files) has how it works and how it is held in.

- **The image** holds LibreOffice 25.2 (Debian 13's `-nogui` packages), the
  fonts Office files are laid out in (Liberation, Carlito, Caladea, whose
  widths are Arial's, Calibri's and Cambria's), and Noto's CJK fonts, so
  that a Chinese slide, simplified or traditional, is drawn in a Chinese
  font, each script in its own forms, and its PDF keeps its text: some 710
  MB unpacked and 305 MB to pull (for amd64), where it was 250 MB and 95
  MB. LibreOffice and what it needs are most of that; Noto's CJK fonts are
  90 MB of it (WenQuanYi's Zen Hei would be 16 MB, but draws the
  characters both scripts share in the mainland's forms alone, and lacks
  many beyond GBK and Big5). It
  still runs as `65532:65532`, with the same entrypoint, and needs nothing
  new of `aishie-runtime-deploy`.
- **What it costs:** one CPU while a file converts, at a lower priority than
  the runtime's own work, and some 400 MB of memory within the 2 GiB it
  may have (`OFFICE_PDF_MEMORY_MB`). A lecture of 38 slides converts in
  about 5 s, 1.5 s of it LibreOffice starting; a file converts once, and
  what was made is kept in the worker's memory (64 MiB). A PDF given as a
  file is cut into parts of ten pages (`PDF_PART_PAGES`), so that a long
  deck does not cost a model a hundred thousand tokens a turn; each part
  is a PDF of its own, in a second or less.
- **How the model sees it:** a file whose PDF Core keeps already (its
  rendition, [below](#office-files-previewed-as-pdf-renditions)) is given
  as that PDF, fetched from Core and not converted here; to a model that
  takes files, even where LibreOffice is missing. Otherwise the first
  question about a file converts it and waits for it up to 2 min
  (`OFFICE_PDF_TIMEOUT`), never past half the answer's time left. A file
  not ready by then is given as its text meanwhile, where the runtime
  reads it without LibreOffice, and the model is told to ask again in a
  minute to see its pages; a file LibreOffice cannot open (damaged,
  protected by a password) is said so.
- **What it may reach:** nothing outside the file. LibreOffice runs as OCR's
  programs do (held in memory, time and what it may write, killed with its
  children at its timeout, nothing of the runtime's environment), from a
  fresh profile each time that blocks every link out of a document, gives
  it a proxy nothing listens at, and turns off active content and macros:
  a picture a document links on a web server is never fetched.
- **The knobs** (in the env file, [below](#the-env-file)): `OFFICE_PDF`
  (`auto`, `on` or `off`), `OFFICE_PDF_TIMEOUT` (2m), `OFFICE_PDF_MAX_PAGES`
  (300), `OFFICE_PDF_MEMORY_MB` (2048) and `PDF_PART_PAGES` (10).
- **Is it on:** the start's log line says `office` and what runs it, or why
  not, and so does `aishie-runtime check`: `office: LibreOffice 25.2.3.2,
  1 at once, 300 pages a file at most, 2m0s a file` and `pdf parts: 10
  pages a file part, at most`. With `OFFICE_PDF=auto` (the default) and
  LibreOffice missing (a binary run outside the image), it is off with a
  warning, and files are given as before, but for those whose PDF Core
  keeps, which a model that takes files is still given; `OFFICE_PDF=on`
  refuses to start without it.
- **Watching it:** `office_conversions_total{to,outcome}`,
  `office_conversion_seconds{to}`, `office_requests_total{result}` (Core's
  PDFs fetched are `rendition`, those not taken `rendition_failed`: one
  not taken is not fetched again for five minutes),
  `pdf_cuts_total{op,outcome}`, `pdf_cut_seconds{op}`,
  `office_conversions_running` and `office_conversions_waiting` in
  `/metrics`. One log line a conversion, with the start of the file's
  checksum, its format, the outcome, pages, size and time, never its
  text.

### Office files previewed as PDF (renditions)

The site previews every Office and OpenDocument file Core keeps (a
document's file of any kind, and a file a message carries: Word, Excel,
PowerPoint, OpenDocument, RTF) as a PDF, converted once, on the server, by
this runtime: Core queues each file as it is recorded, and what it kept
before (AIShie-Core's migration 0026); the runtime claims them, converts
each with LibreOffice, and hands the PDF back to Core, which serves it to
whoever may read the file. `docs/design.md` §13 has how it works.

- **It is on by default, with nothing to configure** but what the runtime
  has already: LibreOffice (in the image), `CORE_BASE_URL`, and the
  runtime's own credential in Core
  ([below](#the-runtimes-own-credential-in-core)), the one it hosts agents
  with. No AI is involved, it costs the site nothing, and the site's
  administrators have no switch for it. Without the credential, or with
  one Core refuses, it converts nothing, says so once in the log, and
  starts by itself once the credential is there. A Core from before
  renditions has nothing to convert: the log says so, and the runtime
  looks again every 10 minutes.
- **What it costs:** one CPU and up to 2 GiB of memory
  (`OFFICE_PDF_MEMORY_MB`) a file while it converts, one file at a time
  per worker (`RENDITIONS_CONCURRENCY`); a lecture's deck takes a few
  seconds. After an upgrade of Core, the backlog (every Office file
  there was) is converted the newest first, behind every new file. It
  keeps nothing in the runtime's database: Core keeps the queue and the
  PDFs.
- **What it may reach:** Core alone: the file's URL and the PDF's upload
  URL Core gives it (fetched and put with no credential, 100 MiB at most);
  LibreOffice runs as every conversion does
  ([above](#presentations-and-documents-libreoffice)): no network, a fresh
  profile, macros off, held in memory and time.
- **What the models are given:** a model reading an Office file whose PDF
  is done is given that PDF, the one people see in the viewer: the
  runtime fetches it from Core with the agent's own read of the file (a
  URL good for 15 minutes, asked for again when it has lapsed, never
  logged and never shown to the model), at most 50 MiB, keeps it in
  memory as it keeps its own conversions, and gives at most
  `OFFICE_PDF_MAX_PAGES` of its pages. A file whose PDF is still waiting,
  failed or skipped is converted by the runtime itself, as before.
- **What becomes of a file:** done, with its pages; skipped,
  `password_protected` (an encrypted Office Open XML or OpenDocument file),
  `unsupported` (not an Office file at all) or `too_large` (the file past
  100 MiB, or its PDF past what Core takes, 100 MiB by default); failed,
  `conversion_failed` (LibreOffice could not open it: damaged, or an older
  binary file protected by a password) or `timeout`. Core fails a file
  claimed five times and never finished `attempts_exhausted`. Staff send a
  failed one back from the site.
- **The knobs** (in the env file, [below](#the-env-file)): `RENDITIONS`
  (`auto`, the default; `on`, the runtime does not start where it cannot
  make them; `off`), `RENDITIONS_CONCURRENCY` (1, at most 8),
  `RENDITIONS_TIMEOUT` (`5m` a file, 5s to 1h) and `RENDITIONS_LEASE`
  (`10m`, how long Core holds a file for this worker, renewed every half of
  it, 1m to 1h). `OFFICE_PDF=off` turns them off too.
- **Is it on:** the start's log line says `renditions`, and so does
  `aishie-runtime check`: `renditions: on, 1 at once, each in 5m0s at most,
  with LibreOffice 25.2.3.2, with the runtime's own credential in Core
  (CORE_SERVICE_CREDENTIAL)`, or why not.
- **Watching it:** `rendition_jobs_total{outcome,reason}`,
  `rendition_seconds`, `rendition_inflight` and
  `rendition_claim_errors_total{reason}` in `/metrics`. One log line a
  file, with its ids, extension, sizes, pages, outcome and time, never its
  name, its URL or what it holds.

To apply a change to the agents, check it, then either tell the runtime to
read its configuration again, or deploy the image that is running again,
which checks it first and changes nothing if the new version refuses it:

```
aishie-runtime check && docker kill -s HUP aishie-runtime
aishie-runtime-deploy "$(docker inspect -f '{{.Config.Image}}' aishie-runtime)"
```

## Hosted agents

Besides the agents in `/etc/aishie-runtime/agents`, the runtime runs the
agents people host on it themselves, by their ids, from AIShie-Frontend
(`docs/design.md` §11): the registry, kept in the runtime's database with
their owners' keys, and the tokens Core issues the runtime for them,
sealed. It is on whenever `DATABASE_URL` is set; the runtime puts a change
to it in force at once, and `check` lists the hosted agents it would run,
and those it would not, with why. `/status` marks them `hosted`. They run
at `CORE_BASE_URL`, which must be a Core that hosts agents by their ids
(AIShie-Core #52 or later, with its `agent_runtime` service): a hosted
agent runs only while Core hosts it `runtime` and names as its owner the
person who hosted it, and is stopped otherwise, in state `owner_changed`
(its token revoked), or `error` (`mcp_agent`, `agent_suspended`,
`owner_suspended`, `core_too_old`). `check --live` reads them as `run`
does, and tries a hosted agent's model as `run` calls it (below). A hosted
agent whose id is a YAML agent's, or that is the agent in Core a YAML agent
names, does not run: the operator's configuration wins. There is one
runtime for the site: an owner cannot run a runtime of their own for an
agent, since only the runtime holding Core's `agent_runtime` credential is
issued an agent's token. A hosted agent's owner being known, its model may
act for them in the conversations they open, as far as its seats'
permissions allow, unless they turn that off (`tools.writes`, through the
API).

A person who decides an agent's answers may send one back for changes,
with a note (Core since AIShie-Core #68): the runtime writes the answer
again, told what to change and shown the answer they read, and proposes
it naming the one it revises. A runtime from before this release never
answers such a conversation again until its asker writes again, so
deploy it before people are given the choice. Its store's migration 0015
lets an attempt be kept as sent back. Rolling the runtime back to v0.1.0,
the release before, leaves the schema as it is
([below](#deploying-and-rolling-back)), and what was asked of an answer
sent back is lost: v0.1.0 reads such an attempt as one that posted
nothing, and has no sentence for its note in memory, so it answers the
question again without what was asked, naming nothing it revises.
Whoever decides that answer can reject it, saying again what they asked:
v0.1.0 gives a rejection's reason to the attempt after when the agent's
memory is on (`memory.enabled`). Sent back instead, it waits for its
asker to write again, as above. v0.1.0 has no sentence either for a
rejection whose reason this release could not read (a note of kind
`rejected_unread`), and does not tell the attempt after it of that
rejection. Do not take 0015 down to keep what was asked: `migrate down`
takes every migration down, not one, and all of the runtime's state with
them. With Core rolled back to before
AIShie-Core #68 after an answer was sent back, its revision, proposed
over MCP, gives `revises`, an argument that Core no longer takes: Core
refuses the call, and the attempt after is written again naming nothing,
still told what was asked. Over REST, Core ignores the `Revises` header.

### The runtime's own credential in Core

The runtime is a site service of Core's, `agent_runtime`, and holds a
credential of its own (`aissvc_…`), with which it asks Core whether a
person owns an agent and may host it, is issued each agent's token by its
id, and revokes it when the hosting ends; and with which it takes from
Core the Office files to convert to PDF, and hands back the PDFs
([above](#office-files-previewed-as-pdf-renditions)). It is the file
`/etc/aishie-runtime/secrets/core/agent_runtime` (`CORE_SERVICE_CREDENTIAL`,
`secret://core/agent_runtime` by default, the file `core/agent_runtime`
under the secrets' mount), which the compose stack (aishie-deploy) writes
at setup. On a server of this repository's own deploy, issue it on Core's
server and put it there, without it ever reaching the screen:

```
docker exec -i aishie-core aishie-core service issue agent_runtime --label runtime --replace 2>/dev/null |
    ssh root@runtime-host 'install -D -g 65532 -m 640 /dev/stdin /etc/aishie-runtime/secrets/core/agent_runtime'
```

(`aishie-core service issue` prints the credential alone on standard
output, and what it did on standard error; `--replace` revokes the
service's other credentials, which is how it is rotated: the runtime reads
the file at each call, and takes a new one with no restart.) A platform
administrator can issue one from Core's site too (`service.issue_credential`,
scope `agent_runtime`). Without it, or with one Core refuses, no agent
runs: each is in state `error`, reason `runtime_misconfigured`, the log
says why, and they start by themselves once it is there; no Office file
is converted to PDF either, until it is.

### Upgrading to hosting by id

Core's migration 0025 (AIShie-Core #52) made every agent whose token
was declared as answering in the site a `runtime` agent, that token the
runtime's, and every other agent `mcp`. Before deploying a runtime that
hosts by id: issue its credential (above), and give every YAML agent its
`core.agent_id` in place of `core.token_ref` (`check` refuses the old
form), its agent in Core being `runtime`. Then `migrate up` (the deploy
does it) adds what lets a hosted agent's row hold no token. At each
hosted agent's first start, the runtime is issued its token in place of
the one its owner pasted, which that revokes; a site with many is paced
within Core's limit. A hosted agent Core made `mcp` is not run, in state
`error` (`mcp_agent`): its owner deletes it in the front end. There is no
rolling back past this release on its own: the release before takes
pasted tokens, which Core no longer issues an owner for a `runtime` agent,
and cannot read a hosted agent that holds no token (one hosted since, its
model not yet chosen, or paused). Going back means restoring the backup the
deploy took (`deploy-*.dump`), with Core's own rollback.

### The school's AI plan

The school may offer the people who host agents here a model on its own
key, so that they need no key of their own: the `school:` section of the
`runtime:` document. Without it, hosted agents run on their owners' keys
alone. Each offer is a model section, as an agent's `model:` is, with an
id owners choose it by and a label they are shown; its key is a reference
under `secret://school/keys/`, a file on this server that is never stored
in the database, shown, audited or sent to a browser:

```yaml
runtime:
  prices_ref: prices/prices.yaml     # optional; needed only for usd: quotas
  school:
    offers:
      - id: standard
        label: "School AI (Claude Haiku)"
        adapter: anthropic
        model: claude-haiku-4-5
        key_ref: secret://school/keys/anthropic-main
        params: {max_output_tokens: 1500}
    per_owner_day: {answers: 100}     # per person, across all of their agents
    per_asker_day: {answers: 20}      # per asker, per agent and course
    per_day: {answers: 5000}          # optional: a ceiling on the school's key
```

```
install -D -g 65532 -m 640 /dev/stdin /etc/aishie-runtime/secrets/school/keys/anthropic-main
    (paste the key, Enter, Ctrl-D)
aishie-runtime check --live && docker kill -s HUP aishie-runtime
```

`per_owner_day` is 100 answers and `per_asker_day` 20 unless set; each may
also take `usd:`, which the runtime refuses to start without a price table
(`prices_ref` or `PRICES`) that prices every offer. Answers alone need no
prices: an offer's cost is then unknown, as the owner's page says. Days
are UTC: a quota starts again at 00:00 UTC. An offer is held to
`allowed_models` and `denied_models` as any model on the school's key.

An owner chooses an offer in the front end, and may put their own model
and key behind it: once the plan's quota for them (or for the asker, or
the school's ceiling) is spent that day, their own key answers; without
one, the asker is told the school's allowance is used up (in English and
Chinese, or in the language `answer_language` fixes; `on_quota_text` in
`school:` sets the words). Taking an offer out of `school:` leaves the
agents on it on their owners' own models behind it, on their keys; an
agent with none stops, saying the school withdrew its offer
(`offer_withdrawn`), until the offer is back or its owner chooses another.
`check` lists the offers and the quotas; the runtime's administrators
read today's use per owner at `GET
/runtime/api/v1/admin/school-plan/usage`.

An offer of a model at OpenRouter (`base_url: https://openrouter.ai/api/v1`,
behind `openai_chat`, or `anthropic` for OpenRouter's Messages API) may say
which of the upstream providers serving the model OpenRouter may pass its
calls to, and on what terms: `openrouter:`, OpenRouter's own provider
routing, sent with every call made on the offer (`docs/design.md` §3).
Every member is optional; one left out is OpenRouter's default (any
upstream, chosen by price and uptime):

```yaml
      - id: llama
        label: "School AI (Llama 3.3 70B)"
        adapter: openai_chat
        model: meta-llama/llama-3.3-70b-instruct
        base_url: https://openrouter.ai/api/v1
        key_ref: secret://school/keys/openrouter
        openrouter:
          data_collection: deny            # only upstreams that keep no data
          zdr: true                        # stricter: zero-data-retention endpoints alone
          require_parameters: true         # only upstreams that take every setting, tools included
          only: [groq, together, deepinfra]
          order: [deepinfra/turbo, groq]   # tried first; allow_fallbacks: false tries no other
          quantizations: [fp8, bf16, unknown]
          preferred_max_latency: {p90: 3}  # seconds; preferred_min_throughput is tokens a second
          max_price: {prompt: 1, completion: "2.50"}   # dollars per million tokens
```

A slug is OpenRouter's (`groq`, `deepinfra/turbo`, `google-vertex/us-east5`);
one with no `/` names every endpoint of its provider. `sort: price`
(`throughput`, `latency`, `exacto`) orders the upstreams strictly instead of
`order`, never beside it. The routing is checked as the configuration
loads, with no network (a slug's shape, not whether OpenRouter lists it
now); `runtime.defaults` may not hold it, and an agent on the offer, or a
course of its, has the offer's exactly. `check` prints it as it is sent.
The key's trial (`check --live`, and an administrator's) sends none.

OpenRouter charges what the upstream provider that answered charges, and
one model's upstream prices differ. The runtime counts every call at the
price table's price for `openrouter` and the model, whichever upstream
answered. Price the model at the highest price the routing allows, or cap
upstream prices with `max_price` and price the model at the cap. A quota in
dollars then stops spending before the bill passes it, and the costs report
shows at most what was billed, not exactly it.

The runtime's administrators also make offers of their own, set the
quotas, in answers and in dollars, and add the prices those need from the
front end ([below](#what-the-sites-administrators-change)): the plan
owners see is `school:`'s offers and the site's.

Upgrade every worker before an offer is given upstream routing in the front
end: a worker of a release before it reads no routing for the site's
offers, and calls OpenRouter without it (a `data_collection: deny` it does
not send is not enforced). The migration that keeps it adds a column, two
checks that only an offer at OpenRouter holds a routing, and a trigger: an
offer that a worker of the release before moves to another provider loses
its routing, as one this release moves does. So a worker of the release
before still runs beside the new one meanwhile, and after a rollback
([below](#deploying-and-rolling-back)): it leaves each offer's routing in
the store, unsent, and can change, move and delete every offer.

## The API for the front end

AIShie-Frontend manages hosted agents through the runtime's JSON API
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
answers with the audience, and `features` says the API hosts agents by
their ids (`host_by_id`, which needs the runtime's own credential) and takes
their owners' own keys; `/status` is not there, and
on `HTTP_ADDR` it refuses any request a proxy forwarded, so pointing Caddy
at `9090` by mistake exposes nothing. In the compose stack (aishie-deploy),
the stack sets all of this itself.

Through the API, a person hosts an agent of theirs by its id, chooses its
model and gives their own key for it, tries a key, pauses and resumes the
agent, asks for a new token after its owner or an administrator revoked
the one the runtime held, and deletes it. Nobody gives the runtime a token:
what the runtime does with Core on their behalf is with its own credential,
asking Core whether the agent is theirs and may be hosted (an `mcp` agent
may not), and revoking the agent's token when they pause or delete it.
Every change, and every refusal, is in the audit (`docs/design.md`
§11.4), with hints of keys, never the values.

A hosted agent's model is called only at the providers' own endpoints,
which the runtime makes from the provider its owner chose (or the offer of
the school's an administrator made): no one gives it a URL. The runtime also refuses, when it dials, any address that is not on
the public internet (loopback, private, link-local and the cloud metadata
address, carrier-grade NAT, the IPv6 forms that hold an IPv4 address, and
the rest of the reserved ranges, whatever DNS says), and follows no
redirect; `check --live` tries a hosted agent's model the same way. Behind
`EGRESS_PROXY` it dials only the proxy, which then resolves and connects:
the proxy must refuse those addresses itself, or a hosted agent's calls
are only as closed as the proxy is. The administrators' page reads OpenRouter's
public lists of the upstream providers serving a model through the same
client, with no key (`https://openrouter.ai/api/v1`: the model's endpoints,
`/providers` and `/endpoints/zdr`, kept ten minutes): a proxy that
allowlists must let `openrouter.ai` through for it, or the page lists none
and its upstreams are added by slug alone.

### What the site's administrators change

The runtime's administrators (Core's `root` and `admin`, narrowed by
`ADMIN_ACTOR_IDS` when it is set) change these from the front end, through
the API's `admin/` routes (`docs/design.md` §11.5), and every worker puts a
change in force within moments, with no restart and no SIGHUP:

- **OCR:** on or off, and its languages among those installed.
- **The school's plan:** offers of the school's, each a provider's model
  at its own endpoint with a key of the school's, which is tried with the
  model before it is kept, sealed in the database like an owner's key,
  and shown only as its hint (`sk-…3f9a`); the upstream routing of an
  offer at OpenRouter ([above](#the-schools-ai-plan)), set against
  OpenRouter's list of the upstream providers serving its model; an offer
  turned off, changed or deleted; and the quotas a day per owner, per
  asker, and across the school, in answers and in dollars.
- **Prices:** rows of the site's own beside the price file's, which the
  front end lists read-only: a model the file does not price, or a price
  that has changed. A site's row of the same provider, model and `from`
  as a file's stands before it. The site's rows are a table of their own,
  versioned by the second they last changed (`site-20260930T101500Z`):
  the ledger names each cost's version and row, so that a cost recorded
  before a change keeps the price it was recorded at.
- **Tenants' quotas:** a tenant's daily quota on the school's key, in
  answers, dollars or both, in place of `runtime.tenants`', and a hosted
  agents' owner's (`ten_<their Core actor id>`) beside the plan's.
- **Hosted agents' daily budgets by default:** per agent and per asker, in
  answers and dollars, in place of `runtime.defaults`' `budgets.per_agent_day`
  and `budgets.per_asker_day` for the hosted agents. `runtime.yaml`'s
  agents keep the budgets `runtime.yaml` gives them.
- **What things cost:** the ledger's model calls in dollars, by day,
  tenant, agent, model or key, over at most a year at a time, the
  transcriber's a line of their own.
- **The transcriber** ([below](#transcribing-the-courses-files)): on or off,
  the plan's offer it transcribes with, its limits, and the service
  credential it claims with.

What stays the operator's, in the env file and `runtime.yaml`:

- **The ceilings:** `OCR=off`, or OCR's programs missing, is off whatever
  the site says; `OCR_LANGUAGES` is the languages until the site chooses,
  and the other `OCR_*` knobs are the env file's alone. So is
  `TRANSCRIBE=off` for the transcriber.
- **`school:`'s offers**, and their keys as files under
  `secret://school/keys/`: the site shows them, read-only, and cannot make
  an offer of the same id. A server of the school's own (a gateway, vLLM,
  Ollama) is offered here alone: the site offers a provider's own
  endpoints, as owners choose them.
- **`allowed_models` and `denied_models`**, which hold the site's offers
  too (an offer they no longer allow is held back from owners),
  `on_quota_text`, and the budgets of one answer (turns, tool calls,
  tokens, time).
- **The defaults the site's settings stand in place of:** the price file
  (`PRICES` or `prices_ref`), `school:`'s quotas, `runtime.tenants` and
  `runtime.defaults`' daily budgets. A site's setting reset in the front
  end takes the operator's again. `runtime.yaml`'s own agents and offers
  are held to the price file alone as the configuration loads: a quota in
  dollars of theirs needs a price in the file, not the site's.
- **`KMS_KEY_ID`**, the key that seals the site's keys as it seals the
  owners': `keys check` and `keys rewrap` cover them, and a backup of the
  database is no use without it ([below](#the-key-that-seals-secrets)).

The site's quotas stand in place of `school:`'s, answers and dollars,
until an administrator resets them to `school:`'s, which the front end
shows beside them. A quota in dollars needs a price for every model it
holds: the site cannot set one while an offer, or a hosted agent's model,
has none today, nor delete the price one needs, and is told which to
price. An offer turned off or deleted is withdrawn as one taken out of
`school:` is (above). Every change is in the audit, with who made it; a
key never is, but its hint.

### Transcribing the course's files

Core keeps beside each file of a course's material, instructions and
rubrics a text version (文字版, AIShie-Core #43): the file as Markdown, a
page under `## 第 N 頁`, each picture described, which people read and
correct in the front end and the agents' models read in place of the file.
The runtime's transcriber makes them: it claims the files waiting in
Core's queue, has a model of the school's plan transcribe each, a range of
pages at a time, and writes the text back (`docs/design.md` §12). A
version of a document may hold several files (a lecture's slides, its
handout and a sample program; AIShie-Core #49): each has a text version of
its own, and is claimed, transcribed and written back on its own; a Core
before it hands out versions of one file, which are transcribed as
before.
It is off until the site's administrators turn it on; off, nothing of the
runtime changes. A model reading a document is given every file of its
version, each file's text version whenever Core has one done, whether or
not this runtime made it, and the file itself otherwise.

- **It needs** the store in PostgreSQL, `KMS_KEY_ID` and `CORE_BASE_URL`,
  as hosted agents do, and a Core with the transcription service (#43 or
  later). `TRANSCRIBE` in the env file is the ceiling: `auto` (the
  default) runs it when the site turns it on, `off` never, and `on`
  refuses to start where it cannot run (those missing, or Core too old or
  out of reach). LibreOffice (in the image) converts presentations and
  documents; without it they are skipped.
- **Turning it on** (the front end's AI 與文件 → 文件 page, `PATCH
  admin/settings`'s `transcription`): the switch; the offer of the school's
  plan it transcribes with, whose model must take files (PDFs, or pages
  as pictures), on the school's key; the most pages a document may have
  (300), the pages a UTC day across the site (no limit unless set), and
  how many documents at once (2). Its costs are in the ledger as their
  own kind: they count against the plan's ceiling across the school's key
  (`per_day_usd`), not against any owner's or asker's quota, and the cost
  report shows them as a line of their own (文件轉寫). Give the offer's
  model a price, or a ceiling in dollars cannot hold it.
- **The credential.** The transcriber works in Core as the site's
  transcription service, with a credential of its own that works nowhere
  else. An administrator issues it in Core and hands it to the runtime in
  one step of the front end's card (「發放並交給 runtime」): the front end
  issues a credential in Core, gives its token to `PUT
  admin/transcription/credential`, which tries it with Core by a call that
  claims nothing and keeps it sealed like a school's key, and then revokes
  the service's other credentials in Core (or the new one, should the
  runtime refuse it). The token is never shown again, nor logged: only its
  prefix (`aissvc_ab12cd34ef56…`). Replacing it is the same; 「撤銷」 forgets
  it here and revokes it in Core. A credential Core stops taking (revoked,
  expired) stops the claiming, and the card says so, until another is
  given.
- **One worker claims**, whatever the number of replicas: the one holding
  the lease `transcriber` in the database; the others stand by and take
  over within a minute of it stopping.
- **Is it on:** `aishie-runtime check` says `transcriber: off in the site's
  settings; …`, `transcriber: on in the site's settings, with the plan's
  offer "…"`, or why it cannot run; so does the start's log line
  (`transcriber`). The card shows what it does now (運作中, 待命, 受阻 and
  why), today's pages, documents and cost, and its jobs of the last 90
  days (`GET admin/transcription/jobs`), a job a file, the document's id
  and the file's (`file_id`, `position`) beside each.
- **Watching it:** `transcribe_jobs_total{outcome}`, `transcribe_pages_total`,
  `transcribe_inflight` and `transcribe_claim_errors_total{reason}` in
  `/metrics`, and its model calls in `llm_calls_total`, `llm_tokens_total`
  and `llm_cost_usd_total{key_source="school"}`. One log line a file,
  with ids, the outcome, pages, calls and time, never its text or name.

## The key that seals secrets

The keys of hosted agents, which people give the runtime rather than an
operator writing them in files, and the agents' tokens Core issues the
runtime, are kept in its database, sealed
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
| `CORE_BASE_URL` | the Core that hosted agents, those people host rather than an operator writing YAML, run at: `https://lms.example.edu`, within `CORE_BASE_URL_ALLOWLIST`. `setup-server.sh` sets it to the Core it was given. Unset, no hosted agent runs, and each one's state says so. |
| `CORE_SERVICE_CREDENTIAL` | where the runtime's own credential in Core is ([above](#the-runtimes-own-credential-in-core)): `secret://core/agent_runtime` (the default), `env://NAME` or `file://…`, never the credential itself. |
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
| `OCR` | `auto` (the default: on where its programs are, as in the image), `on` (the runtime does not start without them) or `off` ([above](#scanned-documents-ocr)). |
| `OCR_LANGUAGES`, `OCR_MAX_PAGES`, `OCR_DPI` | tesseract's languages (`chi_sim+chi_tra+eng`; the image has only these), the pages of a PDF read (`40`), and the resolution they are rendered at (`300`, from 72 to 600). |
| `OCR_PAGE_TIMEOUT`, `OCR_TIMEOUT`, `OCR_MEMORY_MB` | how long one page may take to render or read (`90s`), one file in all (`15m`), and the memory each program may take (`1024`). |
| `OCR_CONCURRENCY`, `OCR_QUEUE`, `OCR_WAIT` | the files a worker reads at once (`1`, at most 8), those that may wait (`8`), and how long a question waits for a file's text before the model is told to ask again (`5s`; `0` waits not at all). |
| `OFFICE_PDF` | `auto` (the default: on where LibreOffice is, as in the image), `on` (the runtime does not start without it) or `off` ([above](#presentations-and-documents-libreoffice)). |
| `OFFICE_PDF_TIMEOUT`, `OFFICE_PDF_MAX_PAGES`, `OFFICE_PDF_MEMORY_MB` | how long one file may take to convert (`2m`), the most pages a PDF given to a model has, LibreOffice's or Core's (`300`), and the memory LibreOffice may take (`2048`). |
| `PDF_PART_PAGES` | the pages of a PDF given to a model as one file, when it has more: a longer one is given in parts (`10`, and never more than the model's provider takes in a file); the transcriber's ranges of pages too. |
| `TRANSCRIBE` | `auto` (the default: the transcriber runs when the site's administrators turn it on), `on` (the runtime does not start where it cannot run) or `off` (never, whatever the site says) ([above](#transcribing-the-courses-files)). |
| `RENDITIONS` | `auto` (the default: the PDF renditions of the Office files Core keeps are made wherever LibreOffice converts and `CORE_BASE_URL` is set), `on` (the runtime does not start where they cannot be) or `off` ([above](#office-files-previewed-as-pdf-renditions)). |
| `RENDITIONS_CONCURRENCY`, `RENDITIONS_TIMEOUT`, `RENDITIONS_LEASE` | the files a worker converts at once (`1`, at most 8), how long one may take (`5m`, 5s to 1h), and how long Core holds a file for the worker, renewed every half of it (`10m`, 1m to 1h). |

`CONFIG` and `SECRETS_DIR` are set by `aishie-runtime-deploy` to the two
mounts, whatever the file says. There is no `OIDC_*`: people sign in to
Core, which vouches for them to the runtime (`docs/design.md` §11.4).
`aishie-runtime help` lists every setting the image in hand reads.

## Connecting the Deploy workflow

`setup-server.sh` ends by printing three settings. Add them in this
repository's Settings → Secrets and variables → Actions (not Core's):

| Kind | Name | Value |
| --- | --- | --- |
| Variable | `DEPLOY_TARGET_EDGE` | `aishie-deploy@lms-test.example.edu` |
| Variable | `DEPLOY_KNOWN_HOSTS_EDGE` | the server's host key line, as printed |
| Secret | `DEPLOY_SSH_KEY_EDGE` | the whole of `/root/aishie-runtime-deploy-key` |

Then delete `/root/aishie-runtime-deploy-key` from the server. The server
keeps only the public half, in `~aishie-deploy/.ssh/authorized_keys`.

For stable, the names end in `_STABLE`. SSH on a port other than 22 is
`ssh://aishie-deploy@host:2222` in the target and
`[host]:2222 ssh-ed25519 …` in the host key line. Settings added before edge
and stable had those names end in `_STAGING` and `_PRODUCTION`: they are
read, with a warning, until a later release
([below](#settings-from-before-the-rename)).

The key is this repository's alone, and logs in as a user of its own,
`aishie-deploy`, not Core's `deploy`: each repository can deploy only its own
image, and Core's `setup-server.sh` writes `deploy`'s keys whole, so a second
key there would not last.

From then on, every green push to `main` deploys to edge, and a
pre-release tag (`v1.2.3-rc.1`) does too. To try the connection without a
push, go to Actions → Deploy → Run workflow, from `main`, with environment
`edge` and image `ghcr.io/aishie-education/aishie-agent-runtime:edge`. That
is also the way to deploy edge again: re-running the deploy of an older
push to `main` fails once `main` has moved on. (Re-running a pre-release's
deploy, or a Deploy run by hand, still deploys the image it had.) Stable
is deployed only by running Deploy by hand, from a release's tag
([CONTRIBUTING.md](../CONTRIBUTING.md#releasing)).

The key only runs `aishie-runtime-deploy`, but that script deploys any image
of this repository. Anyone with write access to the repository can run a
workflow that reads the secret, or copy the key out. They can also push an
image of their own under this repository's name and deploy it, and the
runtime holds its own credential in Core, the agents' tokens and the
providers' keys. The secret is the repository's, not an environment's, so no
environment's rule narrows that down to a branch or to people: write access
is access to everything the runtime can reach. When someone loses write
access, replace the key, delete any package versions they pushed, and
replace the runtime's credential in Core (`--replace`, which revokes the
agents' tokens with the old one's hosting: the runtime is issued new ones)
and the keys.

To replace the key: on the server, delete `~aishie-deploy/.ssh/authorized_keys`
and any `/root/aishie-runtime-deploy-key*` left, run `setup-server.sh` again
as in step 1, with the server's name and its environment, and put the new key
it prints into the secret it names.

### Settings from before the rename

The environments were called `staging` and `production`, and are `edge` and
`stable` now. Deploy reads each of its settings by the new name first and,
until a later release that removes this, by the old one, with a warning in
the run that names the setting to add; so deploys go on while the settings
are renamed. In this repository's settings, before merging the rename if you
can:

1. **Environments** (Settings → Environments → New environment): make `edge`
   with the rules `staging` has, and `stable` with the rules `production`
   has: its required reviewers, and Deployment branches and tags (`edge`:
   branch `main` and tags `v*`; `stable`: tags `v*` only). **Give `stable`
   production's protection before its first deploy.** GitHub neither renames
   environments nor carries their rules over: the first run that names
   `stable` creates it with no protection at all, and then nothing but
   Deploy's own check that it runs from a stable release's tag stands
   between write access to this repository and the schools' sites.
2. **Variables and secrets** (Settings → Secrets and variables → Actions):
   add each one that is set under its new name, with the same value, then
   delete the old one.

   | Kind | Old name | New name |
   | --- | --- | --- |
   | Variable | `DEPLOY_TARGET_STAGING` | `DEPLOY_TARGET_EDGE` |
   | Variable | `DEPLOY_KNOWN_HOSTS_STAGING` | `DEPLOY_KNOWN_HOSTS_EDGE` |
   | Secret | `DEPLOY_SSH_KEY_STAGING` | `DEPLOY_SSH_KEY_EDGE` |
   | Variable | `DEPLOY_TARGET_PRODUCTION` | `DEPLOY_TARGET_STABLE` |
   | Variable | `DEPLOY_KNOWN_HOSTS_PRODUCTION` | `DEPLOY_KNOWN_HOSTS_STABLE` |
   | Secret | `DEPLOY_SSH_KEY_PRODUCTION` | `DEPLOY_SSH_KEY_STABLE` |

   A variable's value can be copied from its page. A secret's cannot be read
   back: paste the key from wherever a copy is kept or, with none, give the
   server a new key ([above](#connecting-the-deploy-workflow),
   to replace the key), which `setup-server.sh` prints under the new name.
3. Once a deploy to each environment runs without a warning, the
   environments `staging` and `production` can be deleted, with the
   deployments they recorded.

The Deploy form offers `edge` and `stable` alone, as GitHub takes nothing
but a choice's options there; a workflow that calls Deploy with `staging`
or `production` has them taken as `edge` and `stable`, with a warning.
Servers need nothing: `deploy/setup-server.sh` takes `edge` or `stable`, or
their old names until the same later release, only to name the settings it
prints.

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
  An agent `unauthorized` holds a token that was revoked in Core (by its
  owner, an administrator or a migration): a reload (SIGHUP) has the
  runtime issued another for a YAML agent; a hosted agent's owner asks for
  a new one in the front end instead. An agent in state `error` with the
  reason `runtime_misconfigured` waits for the runtime's own credential in
  Core ([above](#the-runtimes-own-credential-in-core)); with `mcp_agent`,
  it is an agent Core hosts `mcp`, which the runtime does not host; with
  `core_too_old`, Core must be upgraded. A hosted agent `owner_changed` is
  one whose owner in Core is not the person who hosted it here (a row from
  before an agent's owner was fixed in Core): its token is revoked, and it
  does not run until its owner in Core hosts it again.
- **Metrics:** `curl -s 127.0.0.1:9090/metrics`, in Prometheus's format, for
  a Prometheus on the same machine, or through an SSH tunnel. The one to
  watch is `presence_gap_seconds`: above 60, Core shows the agents as away.
  `long_polls` is each agent's calls waiting for news now, and
  `long_poll_fallbacks_total{why}` the seats sent back to their polling
  schedule for a while: `early` when Core did not wait (its bound of calls
  waiting per agent or in all reached), `cut` when a long poll did not come
  back, `refused` when Core is older than its catalogue said.
  `draft_writes_total{agent, outcome}` counts the drafts of answers being
  written sent to a Core that takes them: `sent`, `dropped` (too soon, or
  the answer had just gone in, or its question been withdrawn) and
  `failed` (after one retry, or refused);
  a steady `failed` is Core refusing them, which `/status` also shows per
  agent (`drafts`, `draft_writes`). They never hold an answer back.
  `search_requests_total{result}` counts the models' searches of a course's
  materials (`hits`, `none`, `refused`, `unavailable`), and
  `search_files_total{outcome}` the files read into the search's index as
  searches needed them (`text`, `empty`: no text to search, such as a scan
  whose text version is not done, `failed`, `not_yet`: the search's 20 s
  ran out first, left to the next search). The index is in the runtime's
  database (migration 0014, tables `search_file` and `search_passage`):
  the text of the course's files the models read, kept per version,
  dropped as the runtime hears that Core purged the version (by the events
  of a seat that reads drafts, `document.purged`, or of one that writes
  assignments, `document.purged_unreleased`, for instructions or a rubric
  not yet released; or as a search reads its tombstone) and otherwise
  after 30 days unused: a whole document purged in a course where no
  agent here sees those events stays stored, though never searched, for
  up to those 30 days. It needs no extension, so the stack's
  `postgres:18` serves it as it is. Scans and older Office and OpenDocument
  files are searchable only by their text versions, which the transcriber
  makes where it is on. Each answer's first search asks Core what its seat
  may read: one `document_list` and a `document_get` for each document
  listed, at most 100, within the agent's rate limit, at the answers'
  priority. An agent answering many students of courses of many documents
  with the search spends some 100 calls an answer, about five such answers a
  minute at Core's default `RATE_LIMIT_PER_MINUTE` of 600; raise it in Core,
  and `polling.assumed_core_rate_per_min` here, where that is too few
  (`docs/design.md` §2.2).
  `budget_exhausted_total{budget}` counts the budgets answers ran into, and
  `truncated` the answers posted cut short, with `on_truncated_text` after
  them: many of those call for a higher `budgets.per_answer.output_tokens`
  or `wall_clock_s`. `output_tokens` counts too a turn cut off by its cap
  before it wrote anything, which forces the last turn: a model that
  thinks, its thinking counted in its output (DeepSeek's `deepseek-flash`
  and `deepseek-v4-pro`, Claude from Opus 5, Gemini 2.5 and 3, OpenAI's
  reasoning models but GPT-5.1, 5.2 and 5.4), thought through its cap.
  The forced turn asks it for the least thinking it takes, configured or
  not, and never for more than it would do unasked, by what its provider
  documents for its family (design.md §5.3, The forced answer): thinking
  off where it goes off (DeepSeek, GPT-5.5 and later's `none`, Gemini 2.5
  Flash), else the lowest effort (GPT-5's `minimal`, `low` for the o
  series and for Claude from Opus 5, Gemini 3's least level), and
  nothing for a model that already thinks least unasked (GPT-5.1, Claude
  4.5, the Flash-Lite models). That holds directly and through
  OpenRouter, Azure (a deployment named for its model) or Bedrock. A
  model whose family is not documented there (Kimi, GLM and Qwen on
  their own endpoints, GLM and Kimi through OpenRouter, a local server, a
  model newer than the runtime knows) is sent no more than its
  configured effort, and nothing if none is configured, so a model there
  that thinks by default may think through the forced turn too. A
  forced turn that writes nothing is asked once more for a short answer
  (`asked_again` in the answer's log line).
  Answers of such a model often forced call for a higher
  `params.max_output_tokens` and `output_tokens`, or a lower
  `reasoning.effort` where its adapter sends one.
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
  upgrade Core on edge first, and look at `/status` after. The runtime
  long-polls its agents' inboxes where the catalogue it read at start
  offers `wait_s` (Core 2c1fe1b and later), and polls them on a schedule
  otherwise: restart it after upgrading Core for a question to be noticed
  within milliseconds rather than seconds. The same goes for the drafts of
  answers, which the runtime writes, streaming its models' answers, only
  where the catalogue it read at start offers `conversation_draft`.
- **Long polls and proxies:** a long poll holds its request open for up to
  `polling.long_poll_wait_s` (25 s) and is given 15 s more to answer.
  Anything between the runtime and Core, `EGRESS_PROXY` included, must let
  a request run 40 s or more; one that cuts it shorter shows as
  `long_poll_fallbacks_total{why="cut"}`, and the seats then poll on their
  schedule, noticing questions in seconds. Core's own Caddy cuts nothing.
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
  secrets. It holds the runtime's own credential in Core and the providers'
  keys: keep the copy encrypted.
- `/etc/aishie-runtime/secrets/kek/`, the keyring, which is in the copy
  above: keep that copy apart from the database's. The database's backups
  hold the hosted agents' secrets sealed, and the keyring opens them; the
  two in one place are every secret in the clear.

## More than one worker

Workers share agents by lease, in the database, so more than one can run
(each also converts Office files to PDF, as many at once as its own
`RENDITIONS_CONCURRENCY`, Core giving no file to two):
on other servers, each with the same configuration and secrets, and the
same `DATABASE_URL`, which then cannot be `127.0.0.1`. `aishie-runtime-deploy`
and the Deploy workflow handle one server per environment, with its database
on that server; a database elsewhere is backed up by whoever runs it, and
the script refuses it.
