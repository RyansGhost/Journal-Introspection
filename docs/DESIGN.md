# Journal Introspection: design

A private web app for reading a directory of org-mode files, with a timeline of journal entries graded by a local model. Setup and settings are in the [README](../README.org); this document explains how it works and why.

## Goals

- Read journal entries, notes and tables from a directory of org files in a browser.
- Grade each journal entry on a few qualities (mood, energy, anxiety and so on) and chart them over time.
- Stay small and readable: one binary, one database file, a few thousand lines of Go.
- Never write to the org files. They are the source of truth; everything else can be rebuilt.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Language | Go | Static binary, 14 MB `scratch` image, standard-library HTTP and templates |
| Storage | SQLite (`modernc.org/sqlite`, pure Go) | One user and one writer; includes full-text search; no C compiler needed |
| Org parsing | `go-org` | The parser Hugo uses; handles tables, drawers, tags and links |
| Grader | [Kev](https://github.com/jaredpalmer/kev) on a separate machine | Returns calibrated scores for questions with written levels, with no prompt parsing |
| Model | Kev-0.8B | Fits a 16 GB machine; Kev-4B wants 32 GB. Any server with the same API works |
| Charts | Server-rendered SVG | No JavaScript build or chart library; works with scripts off |
| Hosting | Home server, Docker Compose, Caddy for TLS | Org files and the model server are on the same network |
| Access | Public address, password plus authenticator code | Chosen for reach; see Security |

Kev is not an Ollama model. It is a Python server with its own API, `POST /v1/systemone`, and runs on its own machine. Ollama is not used yet.

## Architecture

```
 Emacs ──sync──▶ home server: ORG_DIR (mounted read-only)
                              │
 browser ─TLS─▶ Caddy ─▶ ┌────┴──────────── introspect (one Go process) ┐
                         │ indexer ─▶ SQLite (/data/introspect.db) ◀─ web │
                         │ grader ─────────────────┐                      │
                         └─────────────────────────┼──────────────────────┘
                                                   ▼ LAN
                              model server: kev.serve :8009
```

| Part | Package | Job |
|---|---|---|
| Indexer | `internal/indexer` | Walk the tree, find changed files, split journals into entries, feed search |
| Grader | `internal/grader` | Send ungraded entries to Kev one at a time, store the answers |
| Web | `internal/web` | Login, journal and timeline, file browser, search, status |
| Store | `internal/store` | Open SQLite and apply `schema.sql` |

## Change detection

The indexer scans on start and every `SCAN_INTERVAL` (2 minutes by default). It polls because file-change notifications are unreliable on synced folders and container mounts.

1. Skip hidden paths, non-text files and anything matching `EXCLUDE`.
2. If a file's modified time and size match the stored row, skip it.
3. Otherwise read it and hash it. Same hash means it was only touched: update the stored time and stop.
4. Different hash: re-parse the file, replace its entries, search text and link ids.
5. Files that have disappeared, or are newly excluded, are marked deleted and leave the index.

A first index of 400 files took 160 ms; later scans only stat each file.

## Journal entries

A journal file is `journal/YYYYMMDD.org`. Each level-2 heading (`** 07:37`) starts an entry. Tags and the property drawer are kept as metadata and left out of the text. Text before the first heading, or a file with no headings, is one entry with no time.

Each entry has a hash of its text, and **grades are keyed by `(entry hash, rubric version, grade label)`**. This gives three properties:

- Editing one entry regrades that entry only.
- Changing the rubric or the model regrades everything, and the old grades stay.
- Deleting and restoring a file costs nothing, because the grades are still there.

## Grading

The rubric (`internal/grader/rubric.json`) is a list of Kev questions. Its hash is the rubric version.

| Dimension | Type | Levels |
|---|---|---|
| mood, energy | score | 5, from despairing or exhausted to joyful or energised |
| anxiety, stress, connection, purpose, self-regard | score | 4 |
| rumination, mentions sleep / exercise / conflict | yes/no | probability |
| theme | choice | work, relationship, family, friends, faith, health, money, projects, other |

How the worker behaves:

- One entry at a time, newest first, so recent days appear first and the model server is never flooded. The first pass over a few years of entries runs in the background and resumes after a restart.
- If Kev is unreachable, wait and retry with a growing delay (5 seconds up to 10 minutes). The entry is not blamed.
- If Kev rejects an entry (HTTP 4xx or a malformed answer), park it for an hour, then longer, and move on. The status page lists these.
- Entries over 3,000 words are split on paragraphs to fit Kev's 8,192-token context, and the answers are combined weighted by word count.
- A day's score is the word-weighted mean of its entries.

The scores are a model's reading of the text. They are useful for trends and are not a clinical measure; the page says so, and the chart defaults to a 7-day average so one odd grade does not dominate.

## Data model

```sql
files(id, path, kind, title, mtime, size, sha256, indexed_at, deleted_at)   -- kind: journal | roam | note
entries(id, file_id, date, time, heading, body_org, tags, props_json, word_count, content_hash, seq)
org_ids(org_id, file_id)                       -- :ID: properties, for [[id:...]] links
search USING fts5(path, title, body)           -- every indexed file
grades(entry_hash, rubric_version, model, dimension, score, label, confidence, probs_json)
graded(entry_hash, rubric_version, model, graded_at)
grade_failures(entry_hash, rubric_version, model, attempts, last_error, next_try_at)
sessions(token_hash, created_at, expires_at)
login_attempts(ip, at)
totp_used(step)
```

The first four tables are the index. When their layout changes, `indexVersion` in `store.go` is bumped, the index is dropped on the next start and rebuilt by the first scan. Grades and sessions survive.

## Pages

| Route | Content |
|---|---|
| `/`, `/journal/{date}` | Timeline, year grid and reader for one day (the latest by default) |
| `/files/{path}` | Directory listing or a rendered file |
| `/search?q=` | Full-text search over every indexed file |
| `/status` | What the last scan did, grading progress, Kev errors, a "scan now" button |
| `/login`, `/logout`, `/healthz` | Sign in and out; health probe |

The journal page:

- **Timeline.** One line per quality on a shared low-to-high axis. Mood is shown by default; the others are toggled from the legend. Daily, 7-day or 30-day averaging; 3-month, 1-year or full range. Settings live in the URL, so a view can be bookmarked.
- **Year grid.** One cell per day of the selected year, coloured by mood, red through grey to blue.
- **Reader.** The day's entries as rendered org, with the day's scores beside them.
- Clicking a day on either chart opens it. Left and right arrow keys move between days.

Links between org files (`[[file:...]]` and org-roam `[[id:...]]`) point inside the app when the target is indexed.

## Security

The journal is about the most sensitive data a person has, and this is reachable from the internet.

- **Login.** One user. An argon2id password hash and an authenticator secret come from the environment. Each code works once. Five failed attempts from an address lock it out for fifteen minutes.
- **Sessions.** A random 256-bit token, stored hashed, in a `Secure; HttpOnly; SameSite=Strict` cookie that lasts 30 days.
- **Cross-site requests.** POSTs are refused unless `Sec-Fetch-Site` or `Origin` shows they came from the site itself.
- **Headers.** A strict Content-Security-Policy (no inline scripts or styles, nothing from other sites), no framing, no referrer, no indexing.
- **Files.** Everything is read through `os.Root`, so paths cannot escape the org directory. Pages are served only for files in the index, so an excluded file cannot be listed, opened, searched or linked to.
- **Container.** Runs as a non-root user with a read-only filesystem and no capabilities. The org directory is mounted read-only.
- **Kev** stays on the LAN and is never exposed.

Recommended: also turn on the `basic_auth` block in the `Caddyfile`, so a bug in this app is not the only thing between the internet and the journal.

## Verified and not verified

Verified by tests (`make test`) and by running it on a made-up journal of 250 days:

- Change detection: untouched, touched, edited, deleted and restored files.
- Journal splitting, including tags, property drawers and files with no time headings.
- Grading, regrading on a new label, parked rejections, and outages not counting against an entry.
- Login, single-use codes, rate limit, cross-site refusal, excluded files, path escapes, link rewriting.
- The pages in light, dark and phone layouts.

Not verified:

- **The Docker image and compose files.** They are written but were not built, because the build machine had no Docker daemon.
- **A real Kev server.** Grading is tested against `fakekev`, which copies the response format in Kev's README. The rubric wording has not been tried on real entries and will likely need adjusting.

## Later

- Weekly and monthly prose summaries through Ollama.
- Marking a grade as wrong, to build a small labelled set and fine-tune Kev on your own entries.
- Correlations between qualities, and with sleep and exercise mentions.
- Images in org files.
