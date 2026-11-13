# Journal Introspection — design and implementation plan

## Context

Ryan keeps ~715 daily journal files, ~980 org-roam notes and ~50 topic files in `~/SyncRoot/org`. He wants a tiny private web app to read them, plus a timeline that charts his mental state over time by having a local model grade each journal entry. The repo (`ryansghost/journal-introspection`) is empty, so this is a greenfield build.

Decisions already made:

- **Language:** Go.
- **Grader:** [Kev](https://github.com/jaredpalmer/kev), a Jev-like decision model, running on the Mac mini.
- **Exposure:** public internet behind a reverse proxy, so the app needs real authentication.

Two facts from research that shape the design:

- **Kev is not an Ollama model.** It is a Python server (`python -m kev.serve --run jaredpalmer/kev-4b --port 8009`) with its own API, `POST /v1/systemone`. It runs on the Mac mini beside Ollama. Ollama is only needed for optional prose summaries (phase 6).
- **Kev fits this job well.** You send text plus `score` / `noul` / `choice` questions with written level descriptions, and get back a calibrated score, per-level probabilities and a confidence. No prompt parsing or JSON repair is needed.

## Build status

Phases 1 to 5 are implemented and tested (see "Implementation phases"). Not yet done or not yet verified:

- **Docker image build.** The Dockerfile and compose file are written but were not built, because the build machine had no Docker daemon. The static binary is 14 MB.
- **Real Kev.** Grading is tested against a stand-in server that copies the response shape in the Kev README. It has not been run against a real Kev server, and the rubric has not been checked against real entries.
- **Org links.** `[[file:...]]` and org-roam `[[id:...]]` links are not rewritten to point inside the app.
- **TOTP replay.** A code can be reused within its 90-second window.

## Architecture

```
 Emacs ──Syncthing──▶ host:/srv/org (read-only mount)
                              │
 browser ─TLS─▶ Caddy ─▶ ┌────┴──────────────── introspect (Go, one binary) ┐
                         │ indexer ─▶ SQLite (/data/introspect.db) ◀─ web   │
                         │ grader worker ──────────┐                        │
                         └─────────────────────────┼────────────────────────┘
                                                   ▼ (LAN / Tailscale)
                              Mac mini: kev.serve :8009   (Ollama :11434, optional)
```

One process, three parts:

| Part | Job |
|---|---|
| Indexer | Walks the org tree, detects changed files, parses journal entries into SQLite |
| Grader | Background worker that sends ungraded entries to Kev and stores the scores |
| Web | Login, journal home with timeline and reader, file browser, search, status |

### Stack

- Go stdlib `net/http` (1.22+ pattern routing), `html/template`, `embed` for templates and CSS.
- `modernc.org/sqlite`: pure Go, so `CGO_ENABLED=0` and a `scratch` image (~15–20 MB). Includes FTS5 for search.
- `github.com/niklasfasching/go-org`: the org parser Hugo uses. Renders tables, headings, emphasis, links, properties and tags to HTML.
- `golang.org/x/crypto/argon2` for the password hash. TOTP is ~40 lines of stdlib HMAC.
- Charts are server-rendered inline SVG. No JS build step and no chart library; a few lines of vanilla JS handle hover and click-to-open.

SQLite is the right store: one user, one writer, a few thousand rows. Everything in it is derived from the org files, so the database can be deleted and rebuilt at any time.

## Change detection

The org files are the source of truth and are never written to.

1. On start and every `SCAN_INTERVAL` (default 2 min), walk `ORG_ROOT`. Polling is used instead of fsnotify because inotify is unreliable on bind mounts and synced folders.
2. For each file, compare `(mtime, size)` with the `files` row. If unchanged, skip.
3. If changed, read it and compute a SHA-256. If the hash is the same (Syncthing touched it), update the mtime only.
4. If the content changed, re-parse. For journal files, split into entries and hash each entry body.
5. **Grades are keyed by `(entry_hash, rubric_version, model)`.** Editing one entry regrades only that entry. Changing the rubric or the model regrades everything, and the old grades stay for comparison.
6. Files that disappear are marked deleted; their grades are kept.

## Data model

```sql
files(id, path UNIQUE, kind, title, mtime, size, sha256, indexed_at, deleted_at)
  -- kind: journal | roam | note | other
entries(id, file_id, date, time, heading, body_org, tags, props_json,
        word_count, content_hash)          -- one row per "** HH:MM" block
grades(entry_hash, rubric_version, model, dimension,
       score, label, confidence, probs_json,
       PRIMARY KEY(entry_hash, rubric_version, model, dimension))
graded(entry_hash, rubric_version, model, graded_at)   -- one row per graded entry
grade_failures(entry_hash, rubric_version, model, attempts, last_error, next_try_at)
search USING fts5(path, title, body)       -- all org files
sessions(token_hash, created_at, expires_at)
login_attempts(ip, at)
```

Journal parsing: `journal/YYYYMMDD.org` gives the date; each `** HH:MM` heading starts an entry; tags and `:PROPERTIES:` drawers are kept as metadata and stripped from the text sent to Kev. A file with no time headings is one entry.

## Grading rubric

The rubric lives in `internal/grader/rubric.json` and is compiled into the binary; `RUBRIC` points at a replacement file. A short hash of the file is the `rubric_version`. Each dimension is one Kev question with written anchor levels, for example:

```json
"mood": {"type": "score",
  "instructions": "What is the writer's overall emotional tone in this journal entry?",
  "criteria": ["Despairing or hopeless", "Low, sad or flat", "Neutral or mixed",
               "Content and positive", "Joyful, grateful or elated"]}
```

Starting set (easy to edit later, and edits trigger a regrade):

| Dimension | Type | Notes |
|---|---|---|
| mood | score, 5 levels | Headline line on the timeline |
| energy | score, 5 | Exhausted → energised |
| anxiety | score, 4 | None → overwhelming |
| stress | score, 4 | Work and life load |
| connection | score, 4 | Isolated → close time with people |
| purpose | score, 4 | Aimless → meaningful; includes faith |
| self_regard | score, 4 | Harsh self-criticism → self-respect |
| rumination | noul | Looping on a worry or regret |
| mentions_sleep, mentions_exercise, mentions_conflict | noul | Context markers on the chart |
| theme | choice | work, relationship, faith, health, money, projects, family, other |

Grader behaviour:

- One goroutine, one request at a time, so the Mac mini is never flooded. The first backfill of ~715 days runs in the background and is resumable.
- If Kev is unreachable, retry with backoff. The reader works with no grades at all.
- Entries over Kev's 8,192-token context are split on paragraphs and combined as a word-weighted mean.
- A day's score is the word-weighted mean of its entries. Low-confidence scores are marked "uncertain" in the day panel.
- `KEV_URL` and `KEV_MODEL` are environment variables. Start with `kev-4b` if the Mac mini has 32 GB, otherwise `kev-0.8b`.

The page says plainly that these are a model's readings of the text, useful for spotting trends, and not a clinical measure. The chart defaults to 7- and 30-day rolling averages so one odd grade does not dominate.

## Pages

| Route | Content |
|---|---|
| `/login` | Password + TOTP |
| `/` | Journal home. Top: timeline (mood line, toggles for other dimensions, daily / 7-day / 30-day averaging, range buttons 3m/1y/all) and a calendar heat strip. Below: the reader for the selected day, with previous/next and that day's scores beside it. Clicking the chart loads that day. |
| `/journal/2026-11-13` | Same page pinned to a day (linkable) |
| `/files`, `/files/{path}` | Directory tree and rendered org file, tables included. `.org_archive` renders as org; `.md`, `.txt`, `.lisp` show as plain text. |
| `/search?q=` | FTS5 search over all files |
| `/status` | Files indexed, grading queue, last Kev error, a "rescan now" button |

Visual style: plain, text-first, serif reading column, light and dark themes, no framework.

## Security (public exposure)

The journal is about the most sensitive data a person has, so:

- Single user. `AUTH_PASSWORD_HASH` (argon2id) and `AUTH_TOTP_SECRET` come from the environment. A `introspect hash-password` subcommand generates them.
- Session cookie: random 256-bit token, stored hashed, `Secure; HttpOnly; SameSite=Strict`, 30-day expiry.
- Login rate limit by IP (5 per 15 min) and constant-time comparisons.
- Every route except `/login`, `/healthz` and `/static/` requires a session. Cross-site POSTs are refused by checking `Sec-Fetch-Site` and `Origin`, which with the `SameSite=Strict` cookie replaces a CSRF token.
- Headers: strict CSP (no inline scripts, no third-party origins), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `X-Robots-Tag: noindex`.
- Paths are resolved under `ORG_ROOT` with `os.Root` so traversal and symlink escapes are impossible.
- `EXCLUDE` globs keep files out of the index entirely. Default: `identities.org`, `crypto.org`, `finance*.org`, `invoices/**`, `bib/**`, `data/**`, `tmp/**`.
- Container runs as non-root, read-only root filesystem, `/org` mounted read-only, only `/data` writable.
- Kev and Ollama are never exposed publicly; the container reaches the Mac mini over LAN or Tailscale.

**Recommendation:** even with all of this, put a second gate at the proxy (Caddy basic auth, mTLS, or Tailscale Funnel/Serve). A bug in a hobby app should not be the only thing between the internet and the journal.

## Repo layout

```
cmd/introspect/main.go        flags/env, wiring, subcommands (serve, hash-password, healthcheck)
internal/config/              env parsing
internal/store/               SQLite open, migrations (embedded .sql), queries
internal/indexer/             walk, change detection, journal splitter
internal/orgrender/           go-org wrapper → safe HTML
internal/grader/              rubric loader, Kev client, worker, aggregation
internal/web/                 handlers, auth middleware, SVG chart, templates/, static/
cmd/fakekev/main.go           stand-in Kev server for development
testdata/org/                 small fake org tree, including edge-case journals
Dockerfile  compose.yaml  Caddyfile  .env.example  README.org
```

## Implementation phases

Each phase ends with something that runs.

1. **Skeleton (done).** `go mod`, config, SQLite migrations, `/healthz`, Dockerfile (`golang` build stage → `scratch`), compose file. Check image size.
2. **Indexer (done).** Walk, mtime/size/hash detection, journal splitter, FTS. Table-driven tests on `testdata/org` covering: touch without change, edit one entry, delete a file, no time headings, tags and property drawers.
3. **Reader (done).** Auth (password, TOTP, sessions, rate limit, headers), file tree, org rendering, journal day view with previous/next, search.
4. **Grader (done).** Rubric loader, Kev client, worker with backoff, chunking, day aggregation, `/status`. Tested against an `httptest` fake Kev that returns the README's response shape.
5. **Timeline (done).** SVG chart, rolling averages, dimension toggles, calendar strip, click-to-open, hover tooltip, table view.
6. **Later, optional.** Weekly and monthly prose summaries through Ollama; correlations between dimensions; one-click "this grade is wrong" notes; a small labelled set to fine-tune Kev on your own entries (`kev.train`).

Mac mini setup (documented in the README, done by hand): clone Kev, `uv sync --extra serve`, run `kev.serve` under launchd bound to the LAN or Tailscale address.

## Verification

- `go vet ./... && go test ./...`: indexer change-detection cases, journal splitter, grade keying and regrade on rubric change, auth middleware (unauthenticated requests get 302, rate limit triggers, path traversal is refused).
- `docker build` and confirm the image is under ~25 MB and runs as non-root with a read-only root filesystem.
- End to end with `compose up` against `testdata/org` and the fake Kev: log in, open a journal day, edit a fixture file and see it reindexed and regraded on `/status`, see the point move on the timeline. Screenshot the home page in light and dark with Playwright.
- Against the real Mac mini: grade ten real entries with `kev-4b` and read the scores beside the text to sanity-check the rubric before starting the full backfill.

## Open points to confirm during the build

- **Where the container runs.** If it is a VPS, the org files need a Syncthing node there and Kev needs a Tailscale route to the Mac mini. If it is a home server, both are already local.
- **Mac mini RAM**, which decides `kev-4b` versus `kev-0.8b`.
- **The exclude list** above is a guess from the file names.
