# Berth: a dead simple PaaS for Mac minis — design doc

Oct 24, 2026 · @David Singleton

## Summary

Berth is a small PaaS for Apple Silicon Mac minis: `git push` to GitHub and your app is live on the internet at `app.yourdomain.com` in under a minute, with a Postgres database one command away. It composes four things that already exist and are each very good at one job — Apple's `container` CLI (one micro-VM per app), Caddy (routing), Cloudflare Tunnel (public ingress with no open ports), and GitHub webhooks (deploy trigger) — behind a Dokku-shaped CLI. The whole system lives in `~/.berth`, needs no `sudo` after install, no YAML, and no Docker Desktop.

Target: macOS 26, Apple Silicon. Language: Go (two static binaries, `berth` and `berthd`; same toolchain Caddy and cloudflared use). License: MIT. Name: *Berth* (where a ship ties up); code at github.com/dps/berth.

**Status (Oct 2026):** implemented as described here and in daily use on one Mac mini (M4, 16 GB, macOS 26.1, Apple's `container` 1.6.1) serving several apps on a real domain: launchd runs berthd, Caddy and cloudflared; the Cloudflare tunnel, GitHub webhooks, Google sign-in, the AI gateway and the admin page have all carried real traffic. Not yet exercised: recovery from a power cut, `berth reboot`, restoring onto a second Mac (`berth init --restore`), and PR previews against a real pull request (see each section). Sections below describe what the code does; findings from building it are written in place.

```bash
brew tap dps/berth https://github.com/dps/berth && brew trust dps/berth && brew install berth   # once
berth init --domain example.com                                       # ~3 minutes
berth create myapp --repo me/myapp                       # per app
git push                                                  # every deploy
```

## Goals and non-goals

Built for one person with one Mac mini on a home connection, hosting 1–30 small apps that are mostly written by coding agents. That user wants Heroku's loop (push, it's live) without Heroku's bill, and never wants to think about nginx, certificates, port forwarding or `docker-compose.yml`.

**Goals**

- Push-to-deploy from GitHub with zero config beyond the repo name.
- One command gives an app a public HTTPS hostname; no ports opened on the router.
- Postgres and Redis as first-class, linkable services with data on the Mac's own disk and in Time Machine.
- Dockerfile-first builds, with generated Dockerfiles for the common stacks so most repos need none.
- Zero-downtime deploys, instant rollback, readable logs — the Dokku feature set people actually use.
- Survives reboots and crashes unattended; the Mac can sit in a closet.
- Small enough that a coding agent can build it from this doc and a human can read the whole codebase in an afternoon.

**Non-goals**

- Multiple hosts, HA, autoscaling. One Mac. Outgrow it → move to Fly/Render; your Dockerfile comes with you.
- A web UI (v1). The CLI and `berth status` are the UI; a read-only status page over the tunnel is a maybe.
- Buildpack compatibility. (Procfile process types other than `web` run as workers; see **Workers and scheduled jobs**.)
- Intel Macs or macOS < 26 — Apple's `container` is Apple Silicon only and effectively requires macOS 26.
- Replacing `container`, Caddy or cloudflared. Berth orchestrates them; it does not reimplement them.

## The developer experience

The whole product is three moments. Everything in later sections exists to make these three short.

**1. Install (once, \~3 minutes)**

```bash
brew tap dps/berth https://github.com/dps/berth   # the formula lives in this repo
brew trust dps/berth                               # Homebrew 7+ refuses untrusted taps
brew install berth                                 # or --HEAD for main
berth init --domain example.com
```

`berth init` walks through: check Apple Silicon + macOS 26 + `container` installed (offers `brew install container` if not; it is a Homebrew formula, not a cask); `container system start`; install Caddy and cloudflared via Homebrew if missing; log in to Cloudflare in the browser (`cloudflared tunnel login`, run with a temporary `HOME` so it never touches `~/.cloudflared`; the user picks the zone and Berth checks it is the right one); create a tunnel named `berth` and route `*.example.com` to it; log in to GitHub in the browser with the device flow (the one-time code is copied to the clipboard and the verification page opened) to get a token for cloning private repos and registering webhooks, falling back to `--github-token`, `gh auth token`, or a pasted classic token; install `berthd`, Caddy and cloudflared as launchd agents; offer the `pmset` change and the Caddy CA trust; explain the FileVault/reboot choice; print the `berth doctor` summary. `init` is idempotent: re-running it keeps existing settings and only changes what the flags say. Flags: `--no-tunnel` (LAN only), `--cf-ip-filter`, `--unattended`, `--github-token`/`--cf-token`, `--tunnel-name`, `--skip-github`, `--no-launchd` (run `caddy` and `berthd` yourself) and `--yes` (non-interactive: no prompts, no `sudo` steps).

**2. First deploy (per app, \~1 minute)**

```bash
berth create myapp --repo me/myapp
# → registered webhook on github.com/me/myapp
# → https://myapp.example.com (DNS live in ~30s)
# → building main@a1b2c3d ... ✓ running
```

Starting from nothing, `berth new myapp` does the steps before this one too: a starter app, `AGENTS.md` for the coding agent, a GitHub repo, then `berth create` (see **Starting a new app**).

If the repo has no Dockerfile, Berth detects the stack (Node/Bun, Python, Go, Rust, static) and uses a built-in template; `berth dockerfile myapp` prints it so you can commit and edit it. Need a database?

```bash
berth pg:create mydb
berth pg:link mydb myapp     # sets DATABASE_URL, redeploys
```

**3. Daily loop**

```bash
git push                      # webhook → build → zero-downtime swap; seconds with a warm build cache
berth logs myapp -f
berth config:set myapp OPENAI_API_KEY=sk-...
berth rollback myapp         # previous release back in ~2 s
berth status                  # every app, release, uptime, URL, disk
```

Design rules that keep this tight: every command finishes with the URL or the thing you need next; no command needs `sudo` after `init`; no state outside `~/.berth` and `~/Library/LaunchAgents` (plus `container`'s own image and volume store); every Berth concept maps 1:1 to a Dokku concept so the muscle memory transfers.

## Architecture

Six components, all on the Mac, all talking over localhost. Berth owns two of them (`berth` CLI, `berthd` daemon) and configures the other four.

```
            git push                         visitor
               │                                │
               ▼                                ▼
            GitHub ──webhook──▶  Cloudflare edge (TLS, *.<domain>)
                                         │
                          outbound QUIC tunnel (no open ports)
┌─ Mac mini ─────────────────────────────┼──────────────────────────────┐
│                                   cloudflared                         │
│                     hooks.<domain> │          │ *.<domain>            │
│                                    ▼          ▼                       │
│  berth CLI ──unix socket──▶ berthd :7777   Caddy :7780 / :443 (LAN)   │
│                              │    │  admin API ▲   │                  │
│                              │    └────────────┘   │ 127.0.0.1:<port> │
│                              │ container build/run ▼                  │
│                              └───────────────▶  app VMs               │
│                                                    │ berth network    │
│                                                    ▼                  │
│                                               pg-<name> VM            │
└───────────────────────────────────────────────────────────────────────┘
```

Two flows share the tunnel: a push reaches GitHub, whose webhook travels through Cloudflare to `berthd`; a visitor's request travels the same tunnel to Caddy (`127.0.0.1:7780`), which proxies to the app's published port. `berthd` builds and runs containers with the `container` CLI and tells Caddy about new upstreams over its admin API.

| Component | Role | Owned by | Runs as |
| --- | --- | --- | --- |
| `berth` | CLI. Talks to `berthd` over a unix socket at `~/.berth/berthd.sock` (HTTP with newline-delimited JSON, so long operations stream their output); read-only commands (`status`, `logs`, `releases`, `config:show`, `events`, `doctor`) read state files directly and work when the daemon is down. Interactive commands (`run`, `exec`, `pg:psql`, `redis:cli`) exec `container` directly. | Berth (Go) | invoked by user |
| `berthd` | Daemon. Webhook receiver (`127.0.0.1:7777`), git fetch, build, release, health checks, Caddy and tunnel reconfiguration, crash-restart loop, log tailing, branch polling, nightly backups, Bonjour registration, GC. The only writer of `~/.berth`. | Berth (Go) | launchd LaunchAgent |
| `container` | Apple's runtime. One micro-VM per app or service; images, named volumes, the `berth` virtual network. | Apple | `container system start`, via launchd |
| Caddy | Reverse proxy on `127.0.0.1:7780` for the tunnel, `0.0.0.0:443` for LAN (internal CA) and `0.0.0.0:80` redirecting LAN HTTP to HTTPS. One route per app hostname; berthd loads the whole config through the admin API on `127.0.0.1:2019`. | Caddy | launchd LaunchAgent |
| cloudflared | Outbound tunnel. Ingress: `hooks.<domain>` → berthd, `*.<domain>` → Caddy :7780, catch-all 404. | Cloudflare | launchd LaunchAgent |
| Postgres, Redis | Official `postgres:17` / `redis:7` images on the `berth` network, data in `container` named volumes, published on the network's gateway address for linked apps. | Berth-managed containers | `container run` |

Why a daemon rather than a Dokku-style git hook: GitHub is the source of truth for vibe-coded repos (the agent pushes there), the Mac is behind NAT, and a long-lived process is what you need anyway for health checks and restarts. `berthd` is a single Go process (the `daemon` package is about 6.4k lines; the whole codebase about 22k plus 5k of tests, Oct 2026), no database — state is JSON files and the filesystem.

## Apps, builds and releases

An **app** is a name, a repo (GitHub `owner/name`, any git URL or local path, or none for push-only apps), a branch (default `main`), an env map, and a list of releases. A **release** is an immutable image tagged `berth/<app>:<n>` plus the env snapshot it was started with; `current` points at one of them. Deploy = make a new release, start it, swap traffic, stop the old one. Rollback = start the older release's image with its own env snapshot and do the same swap; `current` then points at it. A config change (`config:set`, `pg:link`, `storage:mount`) makes a new release that reuses the current image with the new env, so it takes seconds and never rebuilds; only `--build-arg` changes force a build.

**Build pipeline** (`berthd`, one deploy at a time per app, concurrent across apps):

1. Fetch: `git fetch` the branch into `refs/remotes/origin/*` of the bare mirror `~/.berth/apps/<app>/repo.git` (remote refs never overwrite `refs/heads`, so branches pushed straight to the Mac survive), then `git archive` the commit (or `<sha>:<path>` for monorepos) into `~/.berth/apps/<app>/build/<sha>-<n>/`. If the fetch fails but the ref exists locally, the deploy continues from the local copy.
2. Choose a Dockerfile: the repo's own if present (path overridable with `berth config:set myapp BERTH_DOCKERFILE=deploy/Dockerfile`), else a template picked by detection order: `bun.lockb`/`bun.lock` → Bun; `package.json` → Node (respects `engines.node`, uses npm, pnpm or yarn by lockfile, runs the `build` script if present, starts with the `start` script or `server.js`/`index.js`); `pyproject.toml`/`requirements.txt` → Python (uv; finds the FastAPI/Flask/Starlette app object and starts it with `uvicorn`, `gunicorn` or `flask run` depending on the dependencies; Django via `gunicorn <project>.wsgi`; else `python main.py`); `go.mod` → Go multi-stage (`./cmd/<name>` if there is no root `main.go`, Alpine runtime so `berth exec` has a shell); `Cargo.toml` → Rust; `index.html` with no manifest → static (Caddy file server image). Detection is one small package (`internal/detect`); every template is a plain file under `templates/` so users and agents can read and copy them. A generated Dockerfile is written into the build directory as `.berth.Dockerfile`.
3. Build: `container build --progress plain -t berth/<app>:<n> -f <Dockerfile> <dir>`; the builder VM's layer cache persists across builds, and so do BuildKit cache mounts, which the generated Dockerfiles use for package-manager downloads, the Go build cache and Rust's `target/` (per app). A code change to the Rust demo redeploys in 4 s instead of recompiling axum and tokio. (`container build -q` hung indefinitely twice on 1.6.1; Berth always uses `--progress plain`.) Build logs stream to the CLI and to `~/.berth/apps/<app>/logs/build-<n>.log`.
4. Release command, if the app declares one (see [Release commands and migrations](#release-commands-and-migrations)): back up the app's Postgres databases, then run the command in a one-off container from the new image with the new env. If it fails, the deploy stops here and the current release keeps serving.
5. Start: `container run -d --name berth-<app>-<n>-<suffix> --network berth -p 127.0.0.1:<slot>:<PORT> --env-file releases/<n>.env --cpus/--memory from config -l dev.berth.app=<app> --init berth/<app>:<n>`. The suffix makes every start unique, so a release can be restarted next to its own running container. `PORT` is injected (default 8080) and `BERTH_*` settings are not passed to the app; the host port is whichever of the app's two slots (allocated from 8001–8999 at the first deploy) is free, so two releases of one app run side by side. Multi-line env values go as `-e` because `--env-file` takes values literally.
6. Health: poll `http://127.0.0.1:<port><healthcheck path>` (default `/`, any 2xx–4xx counts as up, configurable with `BERTH_HEALTHCHECK` and `BERTH_HEALTH_TIMEOUT`, default 60 s). The check fails fast if the container exits. On failure: remove the new container, keep the old one serving, mark the release failed, exit non-zero with the last 50 log lines.
7. Swap: point `current` at the new release; in the background berthd waits `BERTH_DRAIN` seconds (default 5) and stops the previous container. **Deploys never touch Caddy's config**, because every admin-API change, even a one-field PATCH, is a full graceful reload, and reloads drop requests at the margins. The first version reloaded per deploy and lost 4 of 5,172 requests over 20 deploys locally (idle keep-alive connections closed as they were reused; `Connection: close` on the tunnel side fixed that), then 1 of 1,161 through the real tunnel (a fresh cloudflared connection accepted during the listener hand-over). Instead, each app's route uses Caddy's SRV dynamic upstreams: Caddy looks up `_<app>._tcp.berth.internal` every second from a small DNS responder in berthd (`127.0.0.1:7753`, `internal/daemon/dns.go`), which answers with the current release's port; `grace_period: 1h` keeps the last answer if berthd restarts. Moving `current` moves traffic within a second, while the old release finishes its in-flight requests during the drain. (An intermediate design listed both of an app's ports with Caddy's `first` policy; whenever the old release held the preferred port it kept taking new requests for the whole drain, so "v3 is live" was untrue for 5 s in half of all deploys. The Chirp demo showed it: posts went to the old, in-memory release.) Each app still owns two fixed ports (*slots*) that releases alternate between, and a deploy that needs a slot still held by a draining release ends that drain early. Upstream keep-alive is off, and refused connections are retried for 5 s. Results: the new release serves 0.04 s after `config:set` returns, and 0 failures in 1,430 requests through the real tunnel across 20 deploys and a restart.
8. GC: keep the last 5 releases' images (`BERTH_KEEP_RELEASES`), delete older images, their env snapshots and build logs, and mark those releases `pruned` (their metadata stays for `berth releases`; rollback to them is refused with the `berth deploy --ref` to use instead). Stopped containers are always removed, never kept. The build directory is deleted after every build.

**Contract with the app**, deliberately identical to Heroku/Dokku so agent-written code works unchanged: listen on `$PORT` on `0.0.0.0`; read config from env; write only to `/tmp` or a mounted volume; log to stdout. `berth create` prints these four lines.

**App names** are DNS labels (`[a-z0-9-]`, ≤ 63 chars) and must not collide with Berth's own hostnames: `hooks`, `berth`, `www`, and anything starting with `pg-`/`redis-` (container names on the `berth` network) are reserved, and `berth create` rejects them with a one-line error.

**Process model**: one web container per app (the image's command), plus the optional release command, worker processes inside it for the Procfile's other process types, and one-off containers for scheduled jobs (see **Workers and scheduled jobs**). v1 shipped with the web process only.

## Release commands and migrations

Apps that keep their schema in their own startup code break in two ways once deploys overlap: during a swap the old and new releases run side by side, so the new code can't assume it is alone; and a migration that fails halfway leaves a half-started container and a database nobody chose. Berth gives schema changes their own step, between build and start, that runs exactly once per release and gates the swap. It is Heroku's release phase, so the tools agents already know (Prisma, Drizzle, Knex, Alembic, Django, Rails, golang-migrate, a 50-line `migrate.js`) work unchanged; Berth has no migration format of its own.

**Declaring it.** The command lives in the repo, so it is versioned with the code it migrates and dev versions pick it up from the working tree:

```
# Procfile
release: npx prisma migrate deploy
```

`berth config:set myapp BERTH_RELEASE_COMMAND="…"` sets or overrides it without a commit (for apps that can't add a Procfile, or to switch it off for one deploy with `true`). Unsetting the setting falls back to the Procfile line, which every release records (`procfile_release`), so config-only releases still know it. Berth reads only the `release:` line; the image's `CMD` stays the web process.

**Running it.** After the build, before the new container starts:

1. On production, a new build first dumps every Postgres database the app links (`pg_dump -Fc` into `~/.berth/backups/pg/<db>/releases/<app>/<time>-v<n>.dump`, newest 14 kept per app, files `0600`). The release records the dumps and which release was current (`data_of`). A failed dump stops the deploy; `BERTH_RELEASE_BACKUP=off` skips it for databases too big to dump every deploy.
2. `container run --rm --entrypoint /bin/sh berth/<app>:<n> -c "<command>"` with the release's env file, network, mounts, CPU and memory: the same environment the web container is about to get. Output streams to the deploy and to `logs/release-<n>.log` (`berth logs <app> --release [n]`). `BERTH_RELEASE_TIMEOUT` (default 600 s) bounds it; on timeout the container is removed.
3. Exit 0: start, health-check and swap as usual. Non-zero: the release is marked failed with `failed_stage: release`, nothing new starts, the current release keeps serving, and the error summary is the last line of output that mentions an error (the full output is in the log). The deploy output names the backup and the `berth pg:restore` that loads it, in case the command changed some data before failing.

Config-only releases (`config:set`, `pg:link`) run the command again, as Heroku does: the new config may point at a fresh database. They skip the backup, since the code (and so its migrations) hasn't changed. Rollbacks and restarts never run it. The command runs under `sh -c`, so the image needs `/bin/sh`: the generated Dockerfiles all have one, and distroless or `scratch` images need one added.

For an app with SQLite, the running release stops before the release command (see **SQLite**): the command needs the database, which is open in one container at a time. The pre-release backup is then a copy of the closed database, and if the command fails the old release starts again.

**Zero downtime still applies, so migrations must be backward-compatible.** The old release serves until the new one is healthy, and drains for 5 s after, all against the migrated schema. The rule is expand/contract: add a column (nullable or with a default) in one release and use it in the same or a later one; stop using a column in one release and drop it in the next; rename by add–copy–switch–drop. Berth can't check this, but dev versions let an agent find out before production does.

**Rolling back data.** `berth rollback <app> [n] --with-data` swaps the code back as usual, then restores the dump taken while the target release was current (the newest release with `data_of == target`). The swap goes first because old code copes with a newer schema better than new code with an older one. Restoring is exact, not a `pg_restore --clean` (which would leave tables the migration added): Berth dumps the current data to `backups/pg/<db>/` first, drops and recreates the database, reapplies every link's grants (production links, then dev ones, which re-revoke `PUBLIC`'s `CONNECT`), and loads the dump as the links' owner group. The CLI says which databases and how old the backup is, and asks; writes since then are lost unless fetched from that safety dump. If no release command ran while the target was live, `--with-data` refuses and says a plain rollback is enough.

**Dev versions rehearse migrations.** `dev:push` runs the release command against the dev database, never backs up (dev data is disposable), and exits 3 when it fails, with `"stage": "release"` in `--json`. `berth dev:db:copy <app>` copies production's data, then makes a config-only release so the release command runs on the copy: copy, push, look, then commit. An agent's loop for a schema change is: add the migration file, `dev:push`, read the exit code, fix, repeat; production sees it only after a commit.

**Hints, not magic.** When a build has no release command but looks like it uses a migration tool (`prisma/migrations`, `drizzle.config.*`, `knexfile.*`, `alembic.ini`, Django `*/migrations/`, Rails `db/migrate`, or a `migrate.js`), the deploy prints the Procfile line to add. Berth never runs a migration nobody declared.

**Results** (chirp-dev and Chirp on the Mac mini, a Node app with a 50-line `migrate.js` and three SQL files):

- First `dev:push` with the release command: three migrations applied in 0.8 s, live 5.9 s after the command started.
- `dev:db:copy` copied production's chirps (no `schema_migrations` table yet), then the config-only release applied the three migrations on top: the `IF NOT EXISTS` baseline adopted the existing tables. (The first version restarted instead of releasing; the new Chirp, which refuses to start without a migrated schema, crashed. That's why the copy now releases.)
- Production, by `git push` to GitHub: the webhook deploy dumped `chirpdb` (5 KB), applied the three migrations in 0.76 s over the existing tables, and was live 5 s after the webhook arrived; the timeline kept its chirps.
- A deliberately broken `004_broken.sql` (adds a column, then selects from a missing table): exit 3 in 4 s with `"error": "release command failed: Error: 004_broken.sql: relation \"no_such_table\" does not exist"`, the column absent (the migration's transaction rolled back), v8 still serving. The next push: `schema up to date`.
- The exact restore behind `--with-data` (`internal/services/live_test.go`, `BERTH_LIVE=1`, against a throwaway Postgres): after a dump, a "migration" that adds a column and a table and a write, then `Replace`, the data is the dump's, the column and the table are gone, tables are owned by `<db>_rw`, the app's login can write, a safety dump exists, and the dev login is still refused `CONNECT`. Rolling Chirp's production back with data was left for its owner to try.

## Deploy triggers

GitHub webhook is the primary trigger; polling is the no-config fallback; `git push berth` and `berth deploy` exist for people who want Dokku's exact feel or are offline. All four end in the same `berthd` deploy job.

| Trigger | How it reaches berthd | Latency | Setup |
| --- | --- | --- | --- |
| GitHub webhook | `POST https://hooks.<domain>/github/<app>` through the tunnel; HMAC-SHA256 checked against a per-app secret | \~2 s | automatic: `berth create --repo` registers it with the GitHub token from `init` |
| Polling | every 60 s for every app with a repo: GitHub repos via the API (`/commits/<branch>` with ETag, so nearly free), other git URLs via `git ls-remote` | ≤ 60 s | always on, so it is the fallback when the token lacks webhook rights, with `--no-webhook` or `--no-tunnel`, and the safety net if a webhook is missed |
| `git push berth main` | a `berth` git remote whose URL is `ssh://<user>@<mac>.local/<home>/.berth/apps/<app>/repo.git`; its `post-receive` hook runs `berth deploy <app> --ref <sha>`, so the build streams back to the pusher | \~1 s | `berth git:remote myapp` adds the remote (`--host` for another SSH host name); needs Remote Login on the Mac. Apps created without `--repo` deploy only this way or with `berth deploy` |
| `berth deploy myapp [--ref sha]` | CLI → unix socket | immediate | none; also redeploys with no code change after `config:set` |

**Webhook details**: `berth create` calls `POST /repos/{owner}/{repo}/hooks` with events `push`, a generated secret stored in the app's state file, and content type JSON. berthd ignores pushes to other branches, tag pushes, and deletes. It records the last 50 delivery ids so a GitHub redelivery is idempotent. Bursts coalesce: while a deploy is queued, further pushes are absorbed, and the queued deploy builds the branch head. `berth create --branch staging` plus a second app name gives a staging environment from the same repo.

**Private repos**: the GitHub token from `init` (scopes `repo`, `admin:repo_hook`) is used for `git fetch` over HTTPS, passed as an HTTP extra header through `GIT_CONFIG_*` environment variables so it never appears in a process list. Users who prefer SSH deploy keys can `berth app:set-key myapp`: berthd generates an ed25519 key, adds it to the repo as a read-only deploy key if the token allows, and otherwise prints it to add by hand.

**Monorepos**: `berth create api --repo me/mono --path services/api` builds from a subdirectory and only deploys when a push touches that path (from the webhook's file lists, or `git diff` between polled heads).

## Networking

Every app gets `<app>.<domain>` on the public internet and `<app>.<mac-hostname>.local` on the LAN, with no router changes and no certificates to manage. Three layers:

**Cloudflare Tunnel (public ingress).** `berth init` runs `cloudflared tunnel login`, `cloudflared tunnel create berth`, and `cloudflared tunnel route dns berth '*.<domain>'` (one wildcard CNAME, so new apps need no DNS step). berthd writes `~/.berth/cloudflared.yml`:

```yaml
# the same ingress is published as the remote configuration
tunnel: <id>
credentials-file: ~/.berth/secrets/cloudflared/<id>.json
ingress:
  - hostname: hooks.<domain>
    service: http://127.0.0.1:7777
  - hostname: "*.<domain>"
    service: http://127.0.0.1:7780
  - service: http_status:404
```

TLS terminates at Cloudflare's edge; the tunnel is an outbound QUIC connection, so the Mac listens on nothing publicly. Cloudflare's free plan limits are generous for this use (100 MB uploads, no bandwidth cap for HTML; video/large-file hosting is a non-goal).

**New apps never touch the tunnel.** The wildcard is one DNS record and one ingress rule, so `myapp.<domain>` already resolves to the tunnel and already lands on Caddy the moment the app exists; `berth create` adds only the Caddy route. cloudflared never learns app names.

**Custom domains are the only tunnel change** (`berth domains:add myapp www.other.com`), and Berth uses Cloudflare's *remotely managed* tunnel configuration for them: berthd calls `PUT /accounts/{account}/cfd_tunnel/{tunnel}/configurations` with the new ingress list, and cloudflared picks it up live, with no restart and no dropped connections. This only works if cloudflared runs with the tunnel's token (`cloudflared tunnel run --token-file secrets/cloudflared/<id>.token`): run with a credentials file and `--config`, it stays on the local file (its `/config` metrics endpoint reports version -1) even though the API accepts the update, which the first real install showed. `berth init` publishes the ingress, saves the token and installs cloudflared in token mode (`config.json` `tunnel_remote: true`); `berth doctor` checks the running version. Without API access it falls back to the local file and a restart. A failed configuration update (the Cloudflare API does time out) doesn't fail the command: berthd retries it every 10 s until it succeeds. `domains:add` refuses to replace an existing DNS record for the hostname (for example a `www` pointing at Cloudflare Pages) unless given `--force`.

**No hand-made API tokens.** The cert that `cloudflared tunnel login` writes contains an API token scoped to the zone the user picked in the browser, plus the zone and account IDs. Berth keeps one cert per zone in `secrets/cloudflared/zones/<zone>.pem` and uses it for everything: `cloudflared tunnel --origincert <cert> route dns` for CNAMEs, and the cert's token for the remote tunnel configuration and for deleting records. A custom domain in another zone needs that zone connected first: `berth cf:login other.com` opens the browser again. A domain whose DNS is elsewhere gets its CNAME printed instead. If remote management is unavailable at `init`, Berth uses a locally managed config instead: it rewrites `~/.berth/cloudflared.yml` and restarts cloudflared through launchd, which drops in-flight connections for a second or two. A scoped API token (`--cf-token`) is needed only for the webhook IP filter (`Zone · WAF · Edit`), which the login token cannot do.

**Caddy (routing).** One Caddy, configured entirely through its admin API so berthd never rewrites a Caddyfile. Caddy listens on `127.0.0.1:7780` for the tunnel rather than `:80`: macOS lets unprivileged processes bind ports below 1024 only on the wildcard address, so `127.0.0.1:80` needs root while `:443` and `:80` on all interfaces do not. Each app is a route: `host == <app>.<domain> or <app>.<hostname>.local → reverse_proxy 127.0.0.1:<port>`. A `--private` app's route matches only the `.local` hostname; since the wildcard tunnel ingress still delivers `<app>.<domain>` to Caddy, that request falls through to Caddy's catch-all 404. Privacy is enforced in Caddy, not in the tunnel. Deploys regenerate and load the whole config (see the swap step above). Caddy also provides: gzip/zstd, HTTP→HTTPS on LAN, access logs per app (`~/.berth/apps/<app>/logs/access.<created>.log`, JSON; see Admin page for why the name carries the creation time), the admin page at `admin.<domain>`, and basic auth for an app with `berth auth:set myapp user:pass`. Caddy's internal CA issues LAN certificates; `berth init` (or `berth trust-ca`) offers to trust the root in the login keychain so Safari on the LAN shows a lock. macOS only advertises the bare `<hostname>.local` over Bonjour, so berthd registers each `<app>.<hostname>.local` with `dns-sd -P` (one small process per app, re-registered if the LAN address changes); `<hostname>` is the Mac's `LocalHostName`.

**`container` network (app ↔ service).** All Berth containers join a user-defined network `berth` (`container network create berth`). Container names do not resolve between containers unless an administrator creates a local DNS domain (`sudo container system dns create`), container IPs change on every restart, and single files cannot be bind-mounted (so no managed `/etc/hosts`). Instead each service publishes its port on the network's gateway address (for example `192.168.66.1:6001`), which every container on the network can reach and the LAN cannot; a linked Postgres is `postgres://myapp:<pw>@192.168.66.1:6001/mydb`. berthd records the gateway and, if the network is ever recreated with a new one, rewrites link URLs and redeploys the affected apps. Apps publish their web port only to `127.0.0.1`; nothing inside a container is reachable from the LAN except through Caddy.

**No-tunnel mode** (`berth init --no-tunnel`): LAN hostnames only. (Berth once also advertised apps as Tailscale Services; it was removed to keep Berth to one way in from outside, the Cloudflare tunnel. Anyone who wants remote private access can still put the Mac on a VPN and use its LAN names.)

## Sign-in

Berth can put Google sign-in in front of any app, so a hobby app gets accounts without writing OAuth code. One Google OAuth client serves every app; berthd runs the login, keeps the sessions, and hands the app the signed-in person as request headers. The provider sits behind a small interface (build the authorization URL, exchange the code for an identity), so GitHub sign-in can be added later without touching the rest.

**Access levels.** Each app has no access policy by default (Caddy proxies it as before, berthd isn't involved). `berth access` turns one on, with a default level and optional per-path rules:

| Level | Behaviour |
| --- | --- |
| `public` | anyone can visit; a signed-in visitor's identity is still passed to the app |
| `signed-in` | any Google account; visitors who aren't signed in are sent to sign in |
| `allow <who...>` | only the listed people: exact emails (`alice@gmail.com`), Google Workspace domains (`@example.com`, matched against the `hd` claim Google vouches for, not the email's text), or named groups (`@family`) |

```
berth access chirp public                   # landing page and timeline open; the app sees who is signed in
berth access chirp /compose/* signed-in     # path rule: a trailing * is a prefix, the longest match wins
berth access chirp /admin/* allow @family you@gmail.com
berth access chirp /compose/* --remove      # drop a rule;  berth access chirp off  removes the policy
berth auth:group family alice@gmail.com bob@example.com
```

Membership is checked on every request, so changing an allow list or a group takes effect on the visitor's next request. Rules match paths only; an app that wants "anyone can read, signed-in users can post" uses `public` and checks the headers on its write endpoints (Chirp does this).

**Request flow.** For an app with a policy, Caddy's route is:

1. delete any `X-Berth-*` request headers the client sent (every app's route does this, policy or not);
2. `/_berth/*` → reverse proxy to berthd's auth server (`127.0.0.1:7781`), Host preserved;
3. `forward_auth`: a `GET /_berth/check` to berthd carrying the original Host, cookies and `X-Forwarded-Uri`. berthd answers 200 with identity headers (empty for an anonymous visitor on a public path), a 302 to `/_berth/login` for a browser that must sign in, 401 for other clients (API calls, non-GET), or a 403 page for a signed-in person who isn't allowed (with "switch account"). Caddy copies the 2xx identity headers onto the request and proxies to the app as usual; anything else goes back to the client. Apps with any non-public rule also get `Cache-Control: private, no-cache` on every response, so Cloudflare never caches a members-only page or asset for someone else.

The app receives `X-Berth-User-Id` (`google:<sub>`, stable for life), `X-Berth-Email`, `X-Berth-Name`, `X-Berth-Picture`, and `X-Berth-Identity`: a 5-minute EdDSA JWT (audience = the hostname) for apps that want to verify instead of trusting the proxy; the public key is at `/_berth/jwks.json`. App ports are published on `127.0.0.1` only, so Caddy is the only way in from the network. An app with a policy also gets `/_berth/login?return=/path`, `/_berth/logout?return=/path` (POST, same-origin only; GET shows a confirm button), which goes back to the path only if it's public: going back to a page that needs sign-in would sign the person straight back in, because Google remembers them and skips its prompt, so they land on `/_berth/signed-out` with a "Sign in again" link instead; and `/_berth/me` (the identity as JSON, for single-page apps). On an app with no policy, once sign-in is set up, `/_berth/*` answers 404 from Caddy with a note to run `berth access <app> public`, rather than reaching the app. *Found by an agent-built app:* it added a "Sign in" link that worked on the dev version (which always has the `@owners` policy) and 404'd in production, which had no policy; `AGENTS.md` now says that sign-in needs a policy, that production starts without one, and that setting it is the user's call. Locally (`npm run dev`, no Berth) the headers are absent and the app sees an anonymous visitor.

**Login.** Google needs a fixed list of redirect URIs and only accepts public hostnames, so there is one sign-in host, `auth.<domain>` by default (already covered by the wildcard DNS record, tunnel rule and Cloudflare certificate). `auth:setup --host signin.<domain>` picks another name directly under the domain; setup refuses a name that is an app or has its own DNS record (the first real install had `auth.cronbon.com` pointing at another server), and `create` and `domains:add` refuse the sign-in host afterwards:

1. `chirp.<domain>/_berth/login?return=/compose` stores `{host, return}` under a random nonce, sets the nonce in a short-lived host-only cookie, and redirects to `auth.<domain>/start?host=chirp.<domain>&n=sha256(nonce)`.
2. `auth.<domain>/start` refuses hosts that aren't Berth apps. If the browser already has a session on the auth host, it skips Google. Otherwise it redirects to Google with `state`, an OIDC `nonce` and PKCE, remembering them under `state` (also set as a cookie on the auth host).
3. Google returns to `auth.<domain>/callback`. berthd checks `state` against the cookie, exchanges the code (with the PKCE verifier and client secret) and reads the ID token it got straight from Google's token endpoint over TLS: issuer, audience, expiry, nonce, and `email_verified` must all check out. It creates a session on the auth host, so the next app's sign-in is a redirect, not a Google prompt.
4. It issues a one-time code (60 s, bound to the app host and `sha256(nonce)`) and redirects to `chirp.<domain>/_berth/callback?code=…`. The app host consumes the code only if its own nonce cookie matches, which stops an attacker from logging a victim into the attacker's account, then sets `__Host-berth_session` (host-only, Secure, HttpOnly, SameSite=Lax) and redirects to `return`, which must be a path on the same host.

The hand-off works for custom domains in other zones and for LAN hostnames too, because only the browser carries the code. Sessions live in `~/.berth/auth/sessions.json` (only hashes of the cookie values), last 30 days and are extended on use; logging out of one app signs the person out of every app and the auth host. `berth auth:sessions` lists them and `berth auth:revoke <email>` ends them.

**Setup** is one command (`berth auth:setup google [--host h]`), because Google has no API for creating OAuth clients: `berth auth:setup google` prints the steps for the Google Cloud console (create a Web application client, add `https://auth.<domain>/callback` as the redirect URI, publish the consent screen; only the `openid email profile` scopes are requested, which need no Google verification) and asks for the client ID and secret. The ID goes in `~/.berth/auth/config.json`, the secret in `secrets/google.client_secret`, the session-signing key in `secrets/auth.key`. Visiting `https://auth.<domain>/` shows who is signed in, which makes a good first test.

Basic auth (`berth auth:set`) remains for sites where a password is simpler; it runs before the access check.

**Hardening from review** (the sign-in review's S-1 to S-5):

- `auth:setup` and `admin:enable` refuse a hostname that is already an app's custom domain. Its DNS record is a tunnel CNAME, so the DNS check passed it, and Berth's route would have silently shadowed the app's.
- Fail closed: a route with a policy but no `auth_addr` (only possible by editing `config.json`) answers 503 instead of serving the app unprotected, and berthd refuses to start with an empty `auth_addr`.
- `allow @gmail.com` (or `@googlemail.com`) is refused with an explanation: `@domain` matches the Workspace `hd` claim, which personal accounts lack, so it matched nobody.
- berthd serves its control socket only after the sign-in, AI and admin servers are up, so handlers never see them half-initialised (a startup data race).
- New sign-ins are limited to 30 per client IP per minute (429), so one client can't fill the table of pending sign-ins (10,000) and crowd out everyone else; existing sessions are unaffected.

## Workers and scheduled jobs

**Workers.** The Procfile's process types other than `web` and `release` (names: lowercase letters and digits, up to 20) are recorded on the release at build time (`processes`, carried over by config-only releases like `procfile_release`). Each runs `app.scale[type]` times (default 1, `berth ps:scale <app> worker=3`, 0 to 10) as more processes in the app's live web container, not in containers of their own (`internal/daemon/workerprocs.go`). That makes an app one VM however many workers it has, gives workers the web process's build, config, mounts and volumes for free, and lets an app with SQLite open its database from a worker, since SQLite's locks hold within one VM but not between two (see **SQLite**). The price: workers share the web container's memory limit (`BERTH_MEMORY`) and CPUs, so a heavy worker means raising those for the app, and a worker can't be sized or isolated separately. (Until Oct 2026 each worker was its own container with its own memory limit. A release still lists any such containers in `workers`, and Berth stops and removes them the next time it touches the app, so upgrading moves workers inside without a deploy.)

How it runs: a worker is `container exec <web container> /bin/sh -c <script>`, kept attached by berthd, which copies its output into `app.log` stamped `v4 worker.2:` (the log tail doesn't see exec'd output; a detached exec's output is lost). Apple's `exec` gives the process the container's user and environment; Berth adds `BERTH_PROCESS=worker.2`, which the app can read and which is how Berth finds the process again. Killing the exec client outright (SIGKILL, or a berthd that dies) leaves the process running inside, so the script first sends TERM to any process in the container whose environment (`/proc/*/environ`) has that `BERTH_PROCESS`, waits 2 s if it found one, then `exec`s the command; stopping a worker sends TERM the same way, KILL after 10 s, and marks the exit as expected. The image needs `/bin/sh`, `tr` and `grep` (Alpine, Debian and Ubuntu bases have them; distroless images don't, and their workers fail to start with the reason in the log). berthd keeps no state for them beyond memory: on start it re-runs every app's workers, and each start replaces a copy the last berthd left.

They follow the live container. Whenever a deploy, restart, rollback, start, stop or scale changes it, the deploy (holding the app's lock) calls berthd's `Workers` hook, which stops the workers in the old container and only then starts them in the new one, so a queue consumer never runs twice at once across a deploy; the gap is a few seconds, plus however long a worker takes to finish after TERM. The supervisor pass (every 10 s) does the same for apps not mid-deploy, which catches a container that went away on its own (an idle dev version sleeping, a crash) and a berthd restart. A worker that exits is restarted with the web process's backoff (1 s doubling to 60 s), and after 10 failures without 10 healthy minutes Berth stops, with a `crash.loop` event, until the next deploy or restart; each restart is a `crash` event and a `[berth]` line in the log. A deploy waits 3 s after starting workers and prints the last lines of any that already exited, because that's usually a config error worth seeing now. `berth status` lists each worker with its uptime; the web container's memory figure includes them.

**Scheduled jobs** use Dokku's `app.json` format, `{"cron": [{"command": "…", "schedule": "…"}]}`, read at build time onto the release (`cron`), so schedules deploy, roll back and reach dev versions with the code, and a malformed entry fails the deploy. Schedules are five-field cron (`internal/cron`: ranges, steps, lists, month and weekday names, 7 as Sunday, and day-of-month OR day-of-week when both are restricted, as in classic cron) or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`, in the Mac's local time.

berthd checks every 10 seconds whether the minute has turned and starts each job whose schedule matches it, on running production apps only: dev versions and previews run jobs only by hand, because a schedule on a copy of production's data could email real people. A run is a fresh container (`dev.berth.role=cron`, removed afterwards) from the current release with its config and mounts, not holding the app's deploy lock, so a long job doesn't block deploys. A job still running when it's due again is skipped (a `cron.skipped` event). `BERTH_CRON_TIMEOUT` (default 3600 s) kills runaways. Output goes to `logs/cron.log` (`v4 cron.1:` lines, with start and finish lines; 10 MB, 3 kept), not `app.log`; `apps/<app>/cron.json` keeps each command's last run (time, seconds, trigger, error, and how many runs in a row have failed). Only the first failure in a row is a `cron.failed` event, which notifies, and the first success after it is a `cron.ok` event that notifies too, with the count. (At first every failed run notified; an every-minute sampler whose sensor dropped off Wi-Fi would have sent sixty an hour.) Runs missed while berthd or the Mac was down are not made up. `berth cron <app>` lists jobs with their next run and last result; `berth cron:run <app> <n>` runs one now and streams its output.

**Results:** deploy tests with the fake runtime check that deploy, config release, scale, stop, start, restart and rollback each hand the live container to the `Workers` hook, that no worker container ever runs, that an older Berth's worker containers are removed, and that a malformed schedule fails the deploy. A daemon test runs workers through a fake `exec` that blocks like Apple's: started by the deploy with its output in `app.log`, shown in status, scaled up and down, restarted after exiting with a `[berth]` line, moved to the new container on restart with the old one's stopped first, and stopped with the app. A live test (`TestLiveWorkers`, `BERTH_LIVE=1`) ran two Python workers inside a real container: their lines reached `app.log`; scaling to one sent `worker.2` TERM, its handler logged it and it stayed stopped (the first run of this test caught Berth restarting it, mistaking the expected exit for a crash); with worker.1's exec client SIGKILLed the process kept running inside, and a fresh daemon replaced it, leaving exactly one; stopping left none. Another daemon test covers schedules firing on their minute only, logs, failure events, `cron`/`cron:run`, and stopped apps not running jobs. On the test Mac, with workers in their own containers (before Oct 2026), a Node app with a worker and two jobs: the worker logged as `v1 worker.1:`, scaled to two, came back 3 s after its container was killed, moved to v2 on redeploy; the `*/2` job fired at the turn of the minute and finished in 0.8 s; a job exiting 3 was reported as `exited with status 3`.

## Dev versions

Every app has a dev version at `https://<app>-dev.<domain>` for trying changes before they are committed, built for coding agents: one command deploys the working tree as it is on disk and returns a result the agent can act on. Production stays git-driven; nothing reaches it except a push to its branch.

**The address exists for every app; the container doesn't.** Until the first push, `<app>-dev.<domain>` shows a "no dev version yet" page. The dash (not `dev.<app>.<domain>`) keeps it one level under the domain, inside the wildcard DNS record, tunnel rule and Cloudflare's free certificate. `berth create` refuses `x-dev` when `x` exists and the reverse; `berth dev:off <app>` removes an app's dev version and its address.

**A dev version is an app.** The first push creates `<app>-dev` (marked `dev_of: <app>` in `app.json`), so every app command works on it: `berth logs chirp-dev -f`, `berth config:set chirp-dev …`, `berth run chirp-dev npm run migrate`, `berth status chirp-dev`, `berth restart chirp-dev`. It builds from the parent's git mirror (`refs/berth/dev`) with the same detection, cache and health checks, keeps 3 releases, has its own slots, env and logs, and is never polled or webhooked.

**Pushing the working tree.** `berth dev:push [app]` snapshots the repo exactly as it is on disk without touching anything the developer sees:

```
cp .git/index $tmp; GIT_INDEX_FILE=$tmp git add -A   # tracked + untracked; .gitignore respected, so .env stays out
git write-tree; git commit-tree -p HEAD -m "berth dev snapshot: 1a510cb + 3 files changed, 40 insertions(+)"
git push <mirror> <commit>:refs/berth/dev            # only new objects travel
```

then asks berthd to deploy that commit to the dev version. The app is the argument, or found from the repo's `berth` remote, or by matching its `origin` against apps' repos. On the Mac the push goes to the mirror's path and the deploy is an RPC; from another machine it goes over the `berth` SSH remote (`berth git:remote`) and the deploy runs as `ssh <mac> berth dev:deploy <app> --ref <sha>`. It is synchronous: build output streams, and on a failed health check the last 50 log lines follow. Exit codes are 0 live, 1 build failed, 2 the app failed to start, 3 the release command failed; `--json` prints `{app, release, status, stage, url, base, dirty, changes, build_secs, error}`. A clean tree deploys `HEAD` itself. A snapshot over 100 MB gets a warning (usually an unignored `node_modules`).

**Data is never production's.**

- Postgres: for each `pg:<db>` the parent links, the dev version gets a database `<db>_dev` in the same server (no second 140 MB container) with its own login and its own owner group `<db>_dev_rw`. `CONNECT` is revoked from `PUBLIC` on both databases, so the dev login can't connect to production's and vice versa. It starts empty; `berth dev:db:copy <app>` replaces it with a copy of production's (`pg_dump --no-owner | psql` with `role=<db>_dev_rw`, so the dev app owns every object and its migrations can alter them), then makes a config-only release so the app's release command migrates the copy.
- Redis: ACLs can't fence numbered databases, so the dev version gets its own small `redis:<name>-dev` container (256 MB), only if the parent links Redis. It was 128 MB until the first real use (the hello app's dev version, Oct 2026): Apple's `container` refuses less than 200 MiB, and the failure left a half-made dev version that the next push would have reused without Redis. Service creation now refuses less than 200 MB up front and removes a service whose container won't start, and a dev version or preview that fails partway is removed so the next push starts over.
- Storage mounts get their own folders under `data/apps/<app>-dev/`.
- SQLite: its own empty volume, `berth-sqlite-<app>-dev`, deleted with the dev version.
- Env: copied from the parent when the dev version is created, minus link URLs (which point at the dev services instead), with `BERTH_KEEP_RELEASES=3`. API keys come along so the dev version works; `berth config:set <app>-dev` overrides. Later parent changes don't flow down; `berth dev:reset <app>` deletes the dev version, its data and its links, and the next push recreates it from the parent.

**Access.** Dev versions run unreviewed code, so with sign-in set up they default to `allow @owners` (the group `auth:setup --owner` creates); without sign-in they are LAN only. Agents can't click through Google, so `berth dev:token <app> [--as test@example.com]` mints a bearer token (shown once, stored hashed on the parent app, valid only on that app's dev hosts, `--revoke` deletes them all). A request with `Authorization: Bearer berth_dev_…` passes the access check; with `--as`, berthd also sends identity headers for that pretend user (`X-Berth-User-Id: dev:test@example.com`), so agents can test signed-in flows with curl or Playwright. `dev:` IDs never collide with real `google:` ones. Dev responses carry `X-Robots-Tag: noindex`.

**Idle dev versions sleep.** The supervisor stops a dev version after 12 hours (`config.json` `dev_idle_hours`) with no deploy and no request (the access log's modification time); its address then says it is asleep. The next `dev:push` (or `berth start <app>-dev`) wakes it.

**For agents.** `berth dev:agents <app>` prints the Berth part of an `AGENTS.md`: how to deploy, the URLs, how to authenticate (`$BERTH_DEV_TOKEN`, never committed), logs, one-off commands, data and migrations, and the rule that production only changes through a commit and push. `berth new` writes the same text into new apps (see **Starting a new app**).

**Not done:** promoting dev to production (commit and push instead; production history stays in git) and waking on a request. Per-pull-request versions are **Pull request previews**.

## Pull request previews

`berth previews:on <app> [--max n]` gives each open pull request into the app's branch its own app, `<app>-pr-<n>`, built from the pull request's head, redeployed on every push to it and destroyed when it closes. It's for reviewing an agent's (or anyone's) change at a real URL before merging.

**A preview is a dev version per pull request.** It is created by the same code (`ensureChild`, with the PR number): `dev_of` is the parent, plus `preview: <n>`; config is copied from the parent minus link URLs; each linked Postgres gets a database `<db>_pr<n>` with its own login and owner group (`CONNECT` revoked as for dev); a linked Redis gets its own small container; storage mounts get their own folders; SQLite gets its own volume. The release command runs on the empty database. Previews get the dev versions' access rule (`@owners` with sign-in, LAN only without), accept the parent's dev tokens, sleep when idle, run workers, and never run scheduled jobs. Names ending in `-pr-<number>` are reserved: `berth create` refuses them.

**Syncing.** `syncPreviews(app)` lists the repo's open pull requests (GitHub API, Berth's token), keeps those whose base is the app's branch and whose head branch is in the same repository, destroys previews whose pull request is gone, and for each remaining one whose newest release isn't already of the PR's head commit: fetches `refs/pull/<n>/head` into the parent's mirror as `refs/berth/pr/<n>`, ensures the preview, and deploys that commit (trigger `pr`). It runs on every `pull_request` webhook delivery (Berth's webhooks now subscribe to `push` and `pull_request`; `previews:on` adds the event to an existing hook) and every 5 minutes from the poll loop, so missed deliveries and closes are caught. One sync per app runs at a time. A commit that already failed isn't retried until the next push.

**Reporting.** Each deploy sets a commit status, context `berth/preview`: pending while building, success with the preview's URL as the link, failure with the `berth logs` command. The first deploy of a preview also comments the URL on the pull request. With `--max` previews already running (default 3), a new pull request gets an `error` status saying so, once per commit.

**Forks never get previews.** A preview runs the pull request's code on the Mac with the app's config (API keys included). A branch in the same repository was pushed by someone with write access, who could already push to the deployed branch; a fork's pull request can come from anyone. So `head.repo` must equal the app's repo.

**Results:** a daemon test against a fake GitHub API and a local repository with `refs/pull/<n>/head`: a same-repo PR gets a preview with copied config, a pending and a success status and one comment; a fork's PR and a PR into another branch get nothing; an unchanged PR isn't rebuilt; a push redeploys without a second comment; over `--max`, one `error` status; closing the PR destroys the preview. Not yet run against a real pull request on GitHub.

## Starting a new app

`berth new <app> [--stack node|python|go|static]` takes someone from an idea to a deployed app that a coding agent can work on, on the Berth Mac or on a laptop (see **Working from another machine**). In order:

1. **Ask Berth about itself.** `berth info` (hidden; JSON: domain, hostname, whether sign-in and the AI gateway are set up) runs locally or over SSH, so the files carry the app's real URLs and only the sections that apply. With no Berth reachable, the URLs are placeholders and the project is still made.
2. **Write the folder.** A starter that follows the app contract, `AGENTS.md`, `CLAUDE.md`, `README.md` and `.gitignore`, then `git init -b main` and one commit. An existing folder is never touched.
3. **GitHub, asking first.** With the GitHub CLI logged in: `gh repo create <user>/<app> --private --source . --remote origin --push`. Without it, the three manual steps are printed. Berth's own GitHub token isn't used: it lives on the Mac and has no repo-creation scope, and creating a repo is the user's act on their account, from their machine.
4. **Berth, asking first.** `berth create <app> --repo <user>/<app>`, locally or over SSH, then (on a laptop) the `berth` git remote, so `berth` commands and `dev:push` in the repo reach the Mac.

Prompts default to yes; with no terminal to answer them they're a no unless `-y` is given, so a script never creates a repo by accident. `--no-github` and `--no-deploy` stop earlier.

**`AGENTS.md` is the instructions file.** It's the cross-agent convention (Codex, Cursor, Copilot, Gemini and others read it); `CLAUDE.md` is the one line `@AGENTS.md`, which Claude Code expands, so there's one source. It covers: running locally; production deploys from pushes to `main`, which only happen when the user asks; `berth dev:push` with its exit codes and the dev token; the app contract and `BERTH_HEALTHCHECK`; logs, status, config and one-off commands; Postgres and Redis, migrations as a `release:` command and the expand-then-contract rule; sign-in headers and paths and the AI gateway when they're set up; and what to ask the user before doing (pushing to `main`, rollback, destroy, production config, `dev:db:copy`). The Berth part is the `berth` block of the same template `dev:agents` prints, so the two can't drift.

**Starters are small and dependency-free where possible:** Node's `http` module, Go's standard library, FastAPI for Python (the one Berth's detection runs with uvicorn), and a static `index.html`. Each answers `/` (which the deploy health check uses) and `/healthz`. They live in `templates/new/<stack>`, rendered with `text/template`; files are stored as `gitignore` and `*.tmpl` so they don't apply to Berth's own repo or join its Go build.

**Results:** a test runs `berth new` from a pretend laptop against fake `ssh` and `gh` and checks every call; another renders every starter and runs Berth's stack detection on it. On the test Mac, each of the four starters was created with `berth new`, deployed with `berth create`, healthy in at most 1 s, and served its page through the tunnel. The owner then made an app with `berth new` on the Berth Mac and had a coding agent build on it, which found the sign-in gap described under **Sign-in** (`/_berth/*` on an app with no policy).

## Working from another machine

The Mac may be in a closet; the editor and the coding agent are on a laptop. So the CLI works there too: **on a machine without Berth** (no `~/.berth/config.json`), a command runs on the Berth Mac as `ssh [-t] [-p port] <mac> 'PATH=… berth <args>'`. The Mac is `$BERTH_HOST` (`you@mac.local`, or an `ssh://` URL), else the host in the current repo's `berth` git remote, so an agent in an app's repo needs no setup. Every argument is single-quoted for the remote shell; the host (from `BERTH_HOST` or the remote URL) can't start with `-` or contain spaces or quotes, and follows `--`, so it can't smuggle in ssh options. `-t` is used only when stdin and stdout are both terminals, so prompts, `logs -f` and `berth run` shells work and `--json` output stays clean. Exit codes pass through (so `dev:push`-style codes and scripts work); ssh's own failure (255) gets a hint about Remote Login and keys. The laptop needs only the CLI; the Homebrew formula requires macOS 26 on Apple Silicon (for `container`), so other machines use `go install github.com/dps/berth/cmd/berth@latest`, which builds for Intel Macs and Linux too.

A few commands do their work locally instead: `new` and `dev:push` (they use local files), `git:remote` (it asks the Mac for the repo URL with `--print`, then adds the remote here, with the host as this machine reaches it), `admin` (it asks the Mac for the address and opens it here), `init`, `uninstall`, `trust-ca`, help and completion. `config:import` sends the local file on stdin (it accepts `-`); any other path argument is a path on the Mac.

Why SSH rather than a network API: SSH is already the trust boundary for `git push berth` and remote `dev:push`, it brings its own keys and host verification, and berthd keeps listening only on a unix socket. The cost is that the Mac's `berth` must understand the command. So every forwarded command carries `BERTH_CLIENT_VERSION=<laptop version>`, and the Mac's `berth` prints a one-line warning on stderr when it differs from its own (which side is older, for release versions; "different builds" otherwise). `upgrade` runs locally: it upgrades the laptop's CLI, then offers to run `berth upgrade` on the Mac.

## AI gateway

Every app can use an LLM, local or cloud, through one gateway in berthd, without holding a provider key. The Mac runs small models itself with Ollama; cloud providers are added with a key the owner gives Berth once. Apps can't tell the difference, and the owner can move an app from a local model to a cloud one (or the reverse) without touching it.

```
app container ──Bearer berth_ai_<token>──▶ Caddy :7790 (container network only) ──▶ berthd 127.0.0.1:7782
                                                                                     ├─▶ Ollama (127.0.0.1:11434)
                                                                                     └─▶ Anthropic, OpenAI, Gemini, OpenRouter, Groq, Mistral, or any OpenAI-compatible URL
```

**What an app sees.** `berth ai:enable <app>` gives the app its own token and sets the variables every OpenAI and Anthropic SDK already reads:

```
OPENAI_BASE_URL=http://<gateway>:7790/v1          ANTHROPIC_BASE_URL=http://<gateway>:7790/anthropic
OPENAI_API_KEY=berth_ai_…                         ANTHROPIC_API_KEY=berth_ai_… (the same token)
```

then makes a config release. The gateway serves the OpenAI-compatible API (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`) for every provider, and passes Anthropic's Messages API through untouched at `/anthropic/v1/messages` (and `count_tokens`) so tools, prompt caching and extended thinking work with Claude. Most providers, Ollama included, speak the OpenAI format natively, so the gateway translates nothing: it checks the token, swaps the model name, adds the real key and relays the response, streaming server-sent events as they arrive. An app that already has its own `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` isn't overwritten without `--replace`; `ai:disable` revokes the token and removes the variables.

**Models are named, not hard-coded.** Apps ask for `provider/model` (`local/qwen3:8b`, `anthropic/claude-sonnet-5-5`, `openrouter/meta-llama/…`) or, better, an alias the owner defines once for every app: `berth ai:alias smart anthropic/claude-sonnet-5-5`. Retargeting an alias takes effect on the next request; nothing redeploys. `ai:setup` creates `default` → the local model. Aliases point at models, never at other aliases.

**Local models run on the host, not in a container.** Apple's containers have no GPU access, so a model in one runs on the CPU several times slower. Ollama runs natively on macOS (Metal) and listens on loopback; only berthd talks to it. `berth ai:setup` installs it with Homebrew and starts it with `brew services` (asking first; Ollama.app works too, as does another machine's Ollama via `ollama_url` in `ai/config.json`), then downloads a model sized to the Mac: `qwen3:8b` (about 5 GB in memory) from 16 GB of RAM, `qwen3:4b` (about 2.5 GB) below that; `--model` picks another and `--cloud-only` skips Ollama entirely. Ollama unloads an idle model after 5 minutes. Local requests are queued (`local_parallel`, default 2, in `ai/config.json`) rather than overcommitting memory; a request that waits 5 minutes gets a 503.

**Why Caddy listens, not berthd.** Containers reach the Mac at the `berth` network's gateway address, where berthd would need a new listening socket, and macOS's application firewall approves listeners per app, not per port: berthd (ad-hoc signed, updated by Homebrew) would prompt or be silently blocked on Macs with the firewall on. Caddy already accepts connections for the LAN. So Caddy's config gains an `ai` server on `:7790` (`ai_port` in `config.json`) that answers only the container network's /24 and loopback (403 for anyone else) and proxies with flushing to berthd's gateway on `127.0.0.1:7782` (`ai_addr`). The server exists only once AI is set up or an app uses it, so Berths without AI open no port; `ai:setup` checks the port is free first, since an occupied port would fail Caddy's whole reload. If the network gateway address changes, apps' `OPENAI_BASE_URL` is rewritten with the service URLs.

**Keys stay in berthd.** Provider keys live in `secrets/ai.json` (0600) and are only ever sent to their provider; apps hold a `berth_ai_` token, stored hashed in `app.json`. Built in: `openai`, `anthropic`, `gemini`, `openrouter`, `groq`, `mistral`; any other OpenAI-compatible service is one command (`berth ai:key together --url https://api.together.xyz/v1`), including key-less servers on the LAN (`berth ai:key lmstudio --url http://studio.local:1234/v1`).

**Limits per app.** `ai:enable` takes `--models` (aliases, `provider/model` or `provider/*`; default any), `--monthly-tokens` (input plus output, per calendar month) and `--rpm`. Over a limit the app gets a 429 in its SDK's error format (OpenAI's or Anthropic's), with a message naming the limit and the command that changes it. Budgets are in tokens, not dollars: providers' prices change and differ by caching tier, and a stale price table would be confidently wrong.

**Usage.** Every request appends a line to `ai/usage-YYYY-MM.jsonl`: time, app, what it asked for, the model that served it, input and output tokens as the provider reported them, latency and status. Prompts and replies are never logged. Streams count too: for OpenAI-style streams the gateway sets `stream_options.include_usage` and, if the app didn't ask for it, drops the final usage-only chunk before it reaches the app; for Anthropic streams it reads `message_start` and `message_delta`. This month's totals are rebuilt from the log at startup, so budgets survive restarts. `berth ai:usage [app]` sums it by app and model; `berth ai:status` shows each app's tokens against its budget.

**Dev versions** get their own token, limited to `local/*`, when created from a parent that uses AI, so an agent looping on `dev:push` can't run up a cloud bill. `berth ai:enable <app>-dev --models 'local/*,smart'` widens it.

**Visibility.** `berth status` adds an `ai` line (Ollama up or down, which model is in memory and its size), and counts that memory in "committed", so the over-commit warning includes it. `berth doctor` checks that the gateway answers through Caddy and, when an alias uses a local model, that Ollama answers.

**Results** (Mac mini M4, 16 GB, Ollama 0.40.11, qwen3:8b):

- `ai:setup` picked qwen3:8b for 16 GB, downloaded it and set `default`. Loaded, it holds 4.9 GB.
- From inside the chirp-dev container: a non-streaming request through `OPENAI_BASE_URL` with the app's token answered in 30 s cold (model load plus qwen3's thinking, 266 output tokens), a streamed one in 13.6 s across 183 events with the extra usage chunk removed, a wrong token got 401, a cloud model with no key got a 503 naming `berth ai:key anthropic`. `ai:usage` showed 4 requests, 84 input and 797 output tokens.
- Chirp's "What's happening?" summary (a 50-line timeline in, two sentences out): 12.9 s on first ask, 0.09 s after (Chirp caches it until the timeline changes).
- Starting Ollama at login: the owner ran `brew services start ollama` (the coding agent that built this isn't allowed to install launch agents). It failed at first because the agent's test `ollama serve` still held port 11434; once that stopped, launchd's retry brought the service up. `ai:setup` could check for a stray Ollama before starting the service.
- Production Chirp after `berth ai:enable chirp`: a three-word reply took 55 s cold, because qwen3 thinks by default (778 hidden output tokens despite `/no_think`) on top of loading the model. Apps that need quick answers from a thinking model should turn thinking off or use a smaller alias.

**Not in v1:** translating between API formats (local models over the Anthropic API, or Claude over OpenAI's beyond what Anthropic's own compatibility endpoint offers), dollar budgets, per-request prompt logging, response caching, and fallbacks between providers.

## Admin page

`https://admin.<domain>` shows the whole Berth on one page, for the people in the `@owners` sign-in group: everything `berth status` prints, live, plus analytics for every app from the request logs Berth already keeps. `berth admin:enable [--host h] [--owner email]` turns it on (it needs sign-in, since it is only for owners), `berth admin` opens it, `admin:disable` turns it off.

**What it shows.**

- *Overview*: health tiles (container system, Caddy, tunnel, free disk, committed memory against physical, the AI gateway or berthd's uptime), any warnings `berth status` would print, the last 24 hours across all apps (requests, visitors, server errors, p50 and p95 latency, data served, each against the 24 hours before), requests per hour stacked by app beside p50 and p95 latency per hour, a table of apps (state, release and commit, requests with a sparkline, 5xx, p50, p95, visitors, memory used against its limit, uptime; dev versions nested under their apps), services with their links and last backup, and recent events. Latency across apps comes from their merged histograms (`Stats.QueryAll`), since percentiles can't be added up app by app; its lines are grey because the requests chart beside it uses the categorical hues for apps.
- *System load*, under the health tiles: the 1-minute load average against the core count, CPU busy, memory in use, swap used and the range's peak load; the 1- and 15-minute load averages; CPU and memory in use on one 0–100% axis; and CPU stacked by app, as a share of one core (Activity Monitor's convention), apps in their usual colors and services in grey. The apps and services tables get a CPU column from the latest sample. A range toggle switches between the last hour (1-minute steps) and the last day (10-minute steps); gaps are when berthd wasn't running.
- *App*: URLs, repo and commit (linked to GitHub), release and uptime; for the last hour, 24 hours or 7 days: requests, visitors (with mobile share), server-error rate, p50/p95/p99 latency, data served and the share from bots and scripts, each with its change; requests over time by status class and latency over time; top pages (IDs folded, so `/api/chirps/12/like` and `/13/like` are one row), referrers, countries and browsers. Tabs: live requests (new ones stream in every 3 s), releases (status, failed stage and error, build and release-command times), the app's log output (following), its setup (sign-in policy, domains, services, mounts, AI limits and usage, config variable *names*), and its events.
- *Events*, filterable by app, kind and text; *AI*: Ollama, models in memory, aliases, providers and each app's tokens against its budget.

It is read-only on purpose: actions stay in the CLI, where they are logged in the shell and need the Mac. Env values, release env snapshots and AI token hashes never reach the page.

**Analytics come from Caddy's access logs, not from the apps.** No script on app pages, no cookie, no extra hop: berthd tails each app's JSON access log (`internal/analytics`), every 5 s, tracking files by inode so rotation doesn't double-count, and keeps 7 days in memory: per-minute counts by status class with bytes and a latency histogram (5 ms … 30 s buckets, percentiles interpolated within a bucket), and per-10-minute top lists and visitor sets. A visitor is a hash of IP and User-Agent, so the count is of distinct browsers, not people; requests whose User-Agent is empty, doesn't start with `Mozilla/` or names a crawler or HTTP library count as bots and scripts and are kept out of visitors. Countries come from Cloudflare's `CF-IPCountry` header (LAN requests have none). Referrers are other sites' hostnames. On start, berthd reads the last 7 days of logs back; a 4 MB log (4,400 requests) loads in 52 ms and a 7-day query takes 44 µs. Nothing about visitors leaves the Mac, and credentials in headers are already redacted in Caddy's logs.

**Access logs are named per app incarnation** (`logs/access.<created>.log`). Building this found that Caddy keeps a log file open for as long as some logger names it, across reloads, and doesn't reliably close the writers of loggers that go away: an app destroyed and created again under the same name went on writing its requests into the deleted file. Chirp had lost a day of request logs that way. A name unique to the app's creation time makes a recreated app's logger a new writer; readers take every `access*.log`, so older logs still count.

**Two locks on the door.** Caddy sends `admin.<domain>` through the same `forward_auth` check as apps, with the policy `allow @owners`, and only then to berthd's admin server on `127.0.0.1:7783` (`admin_addr`). The server also verifies the signed `X-Berth-Identity` header itself: signed by berthd's key, issued for the admin hostname (so a token issued for some app is refused), unexpired. A request that reaches the port any other way gets a 403. The page carries a strict Content-Security-Policy (scripts only from itself, no framing), and everything visitors control (paths, referrers, user agents, app output) is escaped or set as text.

**No build step, no CDN.** The page is three small static files (`internal/admin/ui`: HTML, CSS, ES modules) embedded in berthd and served uncached, so an upgrade is visible on the next load. Charts are hand-drawn SVG: bars no wider than 24 px with rounded data-ends and 2 px gaps between stacked segments, 2 px lines, hairline gridlines on round ticks, a crosshair or band highlight with one tooltip listing every series, and legends always (state is shown with an icon and a word, never color alone). Colors are the reference data-viz palette, with dark mode as its own validated steps; apps keep their color by name order, so refreshing never repaints them. Light and dark follow the system, with a toggle. It works at phone width.

**System load is sampled, not read from logs.** Every 15 s berthd reads the load average, memory pressure (`kern.memorystatus_level`, the free share the `memory_pressure` tool prints; memory in use is 100 minus it) and swap (`vm.swapusage`) with `sysctl`, the CPU from `iostat`'s second one-second report (the first is since boot), and each container's cumulative CPU time from `container stats`; the difference from the last sample, over the time between them, is the cores each app (web and workers) and service used. A counter that went down (a restart) is skipped for one sample. 24 hours of samples (5,760) stay in memory and go to `~/.berth/load.json` every 5 minutes and on shutdown, so an upgrade keeps the history. Each sample costs about 3 s of wall time, mostly `container stats` waiting, and a few milliseconds of CPU. `/api/load?range=1h|24h` averages the samples into steps.

`berth status` takes about 2.7 s on the test Mac (`container stats` dominates), so the admin API serves a cached status up to 4 s old and refreshes it in the background; after the first load, responses take about 25 ms. The page polls the overview every 10 s and the live feed every 3 s, and pauses while the tab is hidden.

**Results** (cronbon.com, three apps): the overview and an app page rendered in WebKit in light, dark and at 390 px wide, from real data: 4.7k requests in 24 hours, hello's load-test burst stacked by app, chirp-dev's 12-second p95 spike when the AI summary ran, the 404s for favicons from a crawler, Chirp's `/api/chirps` as its top page. Anonymous requests to `admin.cronbon.com` got 401 and a forged `X-Berth-Identity` 401 from the sign-in check; berthd's port without a valid identity 403.

## Storage

Postgres is the built-in service, and SQLite gets a volume of its own per app; everything else is a bind-mounted folder. Backups of all three land on the Mac's own disk under `~/.berth`, so Time Machine (or any backup the user already has) covers them with no extra setup: nightly dumps for Postgres, nightly checked copies for SQLite, the folders themselves for app storage.

**Postgres service** (`berth pg:*`, modelled on dokku-postgres):

```bash
berth pg:create mydb            # postgres:17 on the berth network, named volume berth-pg-mydb
berth pg:link mydb myapp        # creates role myapp, sets DATABASE_URL=postgres://myapp:<pw>@<gateway>:<port>/mydb, redeploys
berth pg:unlink mydb myapp
berth pg:psql mydb [psql args]  # psql via container exec; extra args pass through (-c, -tAc ...)
berth pg:backup mydb            # pg_dump -Fc → ~/.berth/backups/pg/mydb/<ts>.dump
berth pg:restore mydb <file>
berth pg:expose mydb 15432      # optional: publish on 127.0.0.1 for TablePlus etc.
berth pg:destroy mydb
```

Implementation: one `container run -d --name pg-mydb --network berth -v berth-pg-mydb:/var/lib/postgresql/data -e PGDATA=/var/lib/postgresql/data/pgdata -p <gateway>:<port>:5432 --env-file <secrets> postgres:17`, 512 MB memory by default (`--memory` configurable). Data lives in a `container` named volume `berth-pg-mydb` (an ext4 image under `~/Library/Application Support/com.apple.container/volumes/`); `PGDATA` points one level down because the volume root holds `lost+found`. A bind mount of a folder under `~/.berth` does not work: Postgres's entrypoint must `chown` its data directory, which virtiofs refuses. So **the live data directory is not a backup**: a file-level copy (Time Machine, `tar`) of a running Postgres is not guaranteed consistent. The dumps are the backup. Nightly `pg_dump` is on by default (03:00 local, keep 14) into `~/.berth/backups/pg/mydb/`, which Time Machine does cover, and a dump also survives Postgres major-version upgrades where a raw data directory does not. `berth backup` excludes the live data directory and takes a fresh dump instead. `berth pg:upgrade mydb --to 18` = dump, new container on a new volume, re-create roles, restore; the old volume is kept until the user deletes it. Multiple apps can link to one database; each link gets its own role (named after the app) and password; every linked role is a member of `<db>_rw`, which owns the `public` schema, and sessions `SET ROLE` to it so tables created by one linked app are usable by the others; `pg:unlink` drops the role's login. The `POSTGRES_PASSWORD` superuser is used only by Berth itself (`psql`, backup, restore, link).

**Redis** follows the same shape (`berth redis:*`, `redis:7` with `--appendonly yes`, `REDIS_URL`) and shipped in v1. Each linked app gets its own Redis ACL user (`redis://<app>:<pw>@…`), kept in `/data/users.acl` on the volume alongside Berth's own `default` login, so `redis:unlink` revokes exactly that app's access (`ACL DELUSER`). Services created before per-app users move to the ACL file on their next `redis:link`. `redis:backup` saves an RDB; `redis:restore` loads it in a throwaway container with AOF off, then turns AOF on so Redis writes a fresh AOF (with AOF on from the start, Redis 7 ignores the RDB and starts empty).

**Folders for apps** (uploads, generated files, caches):

```bash
berth storage:mount myapp /uploads   # → ~/.berth/data/apps/myapp/uploads, bind-mounted read-write
```

Stored in app state and added as `-v` on every release, so it survives deploys and rollbacks. Until Oct 2026 this was also Berth's answer for SQLite (`DATABASE_PATH=/data/app.db`), which turned out to be unsafe: see **SQLite**. A SQLite file in a storage folder still works for an app that never runs two containers at once, but `sqlite:add` is the supported way.

**Backups and restore**: `berth backup` takes a fresh `pg_dump` of every database and a backup of every app's SQLite database (skipping, with a warning, any that would need the app stopped), then tars `~/.berth` minus images, build caches and live Postgres data (state, env, pg dumps, app volumes) into one archive; `berth restore <archive>`, run after `berth init` on a Mac with no apps, unpacks the archive (merging `config.json` so this Mac keeps its own hostname, ports and network gateway), creates fresh volumes, restores the latest Postgres dump, Redis RDB and each app's newest SQLite backup, re-creates roles, and redeploys every app from its repo; berthd's supervisor is paused meanwhile. Apps without a repo come back as config only and need a push. This is also the migration story between machines. The archive excludes git mirrors, build directories and logs.

**Disk**: `berth status` shows free disk; `berth gc` prunes old releases' images past the keep count, dumps past retention and build directories left by interrupted deploys. berthd refuses to deploy below 2 GB free rather than filling the boot volume.

## SQLite

Agents reach for SQLite first, and for a one-Mac PaaS it's often the right database: no service to run, a file per app. `berth sqlite:add myapp` gives an app its own:

```bash
berth sqlite:add myapp [--as SQLITE_URL]   # volume berth-sqlite-myapp at /data/sqlite; DATABASE_URL + SQLITE_PATH; redeploys
berth sqlite:backup myapp [--stop]         # consistent copy → ~/.berth/backups/sqlite/myapp/<ts>.db
berth sqlite:restore myapp <file>          # saves the current one, loads the file (the app stops for a few seconds)
berth sqlite:shell myapp ["select …"]      # sqlite3 (or Python 3.12's shell) inside the app's container
berth sqlite:list
berth sqlite:remove myapp [--delete-data]  # unset the variables; the volume is kept unless --delete-data
```

The app sees `DATABASE_URL=sqlite:////data/sqlite/app.db` (the four-slash absolute form Django's `dj-database-url`, SQLAlchemy and most URL parsers take; `--as` picks another name, and `sqlite:add` refuses to overwrite a variable a linked Postgres already set) and `SQLITE_PATH=/data/sqlite/app.db` for drivers that want a path. Backups, restore and the shell work on that file.

**Why not a file in a shared folder: SQLite's locks don't cross VMs.** Every Berth container is its own VM, and a bind-mounted folder reaches it over virtiofs. A test on macOS 26.1 with `container` 1.6.1: one container opened a database in a host folder and took the write lock (`BEGIN IMMEDIATE`); a second container on the same folder then wrote and committed anyway, in both rollback-journal and WAL mode. Two writers corrupt a SQLite database, and Berth ran two containers on the same files routinely: the old and new release side by side during every zero-downtime deploy, the release command while the old release served, workers beside the web process, and cron jobs. (The host's own `sqlite3` is no safer: its locks don't reach into the VMs either.)

**A volume can only be attached to one VM.** A `container` named volume is an ext4 disk image, and the second container to attach a volume that's in use fails to start: `The storage device attachment is invalid` (Virtualization.framework). So the database lives on a named volume, `berth-sqlite-<app>`, mounted at `/data/sqlite`, and the hypervisor, not Berth's own bookkeeping, guarantees there is never a second writer. A volume's root belongs to root, so Berth makes it world-writable when it creates it (a helper `alpine:3` container runs `chmod 0777`) for apps whose image runs as another user; only that app's VM ever sees it.

**Measured (Oct 2026, same container, Python's sqlite3, 500 one-row commits, 100k-row insert in one transaction, 5k point reads):**

| Where the file lives | Journal | Commits/s | Bulk rows/s | Reads/s |
|---|---|---:|---:|---:|
| the container's own filesystem | WAL, `synchronous=NORMAL` | 299,065 | 1.25M | 427k |
| named volume (ext4 image) | WAL, `synchronous=NORMAL` | 343,839 | 1.22M | 671k |
| host folder (virtiofs) | WAL, `synchronous=NORMAL` | 7,186 | 86k | 701k |
| named volume | rollback, `synchronous=FULL` | 3,254 | 1.78M | 414k |
| host folder | rollback, `synchronous=FULL` | 960 | 285k | 22k |

The volume is as fast as the container's own disk, and 15 to 40 times faster than a host folder for writes. The container's own filesystem would be as fast but is thrown away with the container on every deploy. Whether a `synchronous=FULL` commit inside the VM is on the Mac's disk, or only in the disk image's cache, when it returns hasn't been verified; Postgres's volumes have the same question, and the nightly backups are the answer for both.

**One container at a time.** For an app with SQLite (`app.sqlite` in `app.json`), Berth changes how it runs things:

- *Deploys, rollbacks and restarts stop the running release first* (`takeOver`) instead of starting the new one beside it. If there's a release command, the old release stops before it, so the command can open the database; the pre-release backup is then a plain copy of a closed database (below). If the release command fails or the new container doesn't get healthy, the old release starts again (`giveBack`) and keeps serving; if even that fails, the app is marked crashed and an event says so.
- *Visitors wait instead of getting errors.* During the gap the release's record still names the old container, so berthd's SRV answer still points Caddy at its port; the app's route retries refused connections for 60 s (`try_duration`, 5 s for other apps) and the new port takes over within a second of the swap. A deploy without a release command is down for a VM boot plus the app's start, typically 2 to 5 s; a long migration is longer, and a request still waiting after 60 s gets a 502.
- *Workers can use the database*, because every app's workers run inside its web container (see **Workers and scheduled jobs**). This is why they were moved there: in their own containers they couldn't attach the volume, and the first version of SQLite support ran no workers at all.
- *Cron jobs and `berth run` go inside the running web container* (`container exec`) with its release's config, rather than starting a new one. A job that hits `BERTH_CRON_TIMEOUT` stops being waited for, but the process inside the container isn't killed. A job running when a deploy stops the container is killed with it. When the app isn't running, scheduled jobs don't run (as before), and `berth run` starts a fresh container, which can attach the volume because nothing else has it.

**Backups** go to `~/.berth/backups/sqlite/<app>/YYYYMMDD-HHMMSS.db`, nightly with the Postgres dumps (03:00, keep 14), from `berth backup` and off-Mac backups, and on demand. Each is one ordinary SQLite file:

- *While the app runs*, berthd runs a short script in the app's container: SQLite's online backup API, through `sqlite3 .backup` if the image has it, else Python's `sqlite3.Connection.backup`, to a temporary file it streams out on stdout. The backup API copies a consistent snapshot while the app goes on writing. Berth holds the app's deploy lock meanwhile, so a deploy can't stop the container mid-copy.
- *An image with neither sqlite3 nor Python* (a Go or Node binary on a slim base, say) can't do that. The nightly backup then stops the app for a few seconds (`Paused`), copies the database and its `-wal` file out of the volume with a helper `alpine:3` container (`tar` to stdout), and starts it again; `sqlite:backup` asks for `--stop` first. A copy of a closed database, `-wal` included, is consistent.
- *Every copy is checked on the Mac* before it's kept: the Mac's own `/usr/bin/sqlite3` folds any `-wal` file in (`.backup`), runs `PRAGMA integrity_check` on the copy opened `immutable` (no locks, nothing written beside it), and sets `journal_mode=DELETE`, because the backup API keeps the source's WAL flag in the copy's header and a WAL-mode file can't be opened read-only without its `-shm`. A copy that fails the check is discarded and the backup reports an error.
- *Before a release command*, with the app already stopped, a plain copy goes to `backups/sqlite/<app>/releases/<ts>-v<n>.db` and is recorded with the Postgres dumps, so `berth rollback --with-data` restores it too, and a failed release command prints the `sqlite:restore` line.

**Restore** checks the file first (a SQLite header and a clean integrity check), stops the app, copies the current database aside (`<ts>-before-restore.db`), streams the file into the volume through the helper container (removing any `-wal`, `-shm` or `-journal` left beside the old one, and giving it the old file's owner), and starts the app. `berth restore` from an archive on a new Mac creates each app's volume and loads its newest backup.

**Dev versions and previews get their own empty database** (`berth-sqlite-<app>-dev`, `berth-sqlite-<app>-pr-<n>`, created with them and deleted with them); the release command sets it up. Their config already points at the same path, so nothing else changes. They aren't backed up. `dev:db:copy` doesn't copy SQLite yet; `sqlite:backup` then `sqlite:restore <app>-dev` does the same by hand.

**Results** (Oct 2026): deploy tests with the fake runtime, which now refuses to attach a named volume to a second running container as Apple's does: the old release stops before the new one runs (and before the release command), a failed health check or release command starts the old release again, restarts and rollbacks stop first, cron jobs exec into the web container. Disabling `takeOver` fails four of them. Daemon tests cover `sqlite:add`/`remove`, the refused variable, the route's retry window, an online backup (and a torn copy being rejected), the `--stop` fallback, restore order and the safety copy, and a dev version's own volume. A live test (`BERTH_LIVE=1`, throwaway containers and volume in a temporary Berth home) against `container` 1.6.1: a second VM attaching the volume was refused; three online backups taken half a second apart while a Python app wrote a row every 2 ms in WAL mode as user 1000 were each consistent (893, 1,090, 1,285 rows); after the app stopped, the volume held a 4 MB `-wal` file and the stopped copy had all 2,967 rows; a restore brought back the 893-row copy, owned by 1000:1000, with no stale `-wal`; an `alpine` image was correctly found to have no sqlite3 or Python.

## Config, secrets and the on-disk layout

All state is plain files under `~/.berth`, readable with `cat`, backed up with `tar`, and simple enough that a coding agent can debug Berth by looking at the directory.

```
~/.berth/
  config.json            # domain, tunnel id, github user, ports, defaults, reboot mode
  berthd.sock            # CLI ↔ daemon (in $TMPDIR if ~/.berth's path is too long for a socket)
  caddy.json             # last-applied Caddy config (rebuilt from apps/*/app.json on start)
  caddy/                 # Caddy's storage: the internal CA and LAN certificates
  cloudflared.yml
  events.log             # JSON lines: deploys, restarts, webhooks, backups (berth events)
  logs/                  # berthd.log, caddy.log, cloudflared.log (launchd stdout/stderr)
  secrets/               # mode 0700
    github.token
    cloudflare.token     # optional scoped API token (only for --cf-ip-filter)
    keys/<app>           # SSH deploy keys (app:set-key)
    cloudflared/<tunnel>.json     # tunnel credentials
    cloudflared/zones/<zone>.pem  # browser-login cert per connected zone
    ai.json              # AI provider keys
  ai/
    config.json          # aliases, extra providers, ollama_url, local_parallel
    usage-YYYY-MM.jsonl  # one line per AI request: app, model, tokens, latency (no prompts)
  apps/<app>/
    app.json             # repo, branch, path, private, mounts, sqlite, links, domains, auth, hook secret, poll state
    env                  # KEY=value, mode 0600; the live config
    releases/<n>.json    # sha, image, env snapshot, host port, container, status, timestamps
    releases/<n>.env     # the snapshot as an --env-file, mode 0600
    current              # → release number
    repo.git/            # bare mirror
    build/<sha>-<n>/     # exported tree, deleted after build
    logs/                # build-<n>.log, release-<n>.log, app.log (rotated), access.<created>.log
  services/<pg|redis>/<name>/
    service.json         # image, port, volume, password, per-app link roles
    env                  # the container's --env-file (passwords), mode 0600
  data/apps/<app>/<mount>/
  backups/pg/<name>/*.dump
  backups/redis/<name>/*.rdb
  backups/sqlite/<app>/*.db             # nightly and manual; releases/<ts>-v<n>.db before release commands
  load.json              # the last 24 hours of system load samples (admin page)
```

**Config vars**: `berth config:set myapp KEY=val ...` writes `env` and makes a new release with the current image (`--no-restart` to defer); `config:get`, `config:unset`, `config:show` (masks values ending in `_KEY`, `_SECRET`, `_TOKEN`, `PASSWORD`, and `DATABASE_URL`/`REDIS_URL`, unless `--reveal`; `--shell` prints `KEY=value`), `config:import .env` (`--replace` drops vars not in the file). Berth-controlled vars are namespaced `BERTH_*`: `BERTH_DOCKERFILE`, `BERTH_HEALTHCHECK`, `BERTH_HEALTH_TIMEOUT`, `BERTH_DRAIN`, `BERTH_MEMORY`, `BERTH_CPUS`, `BERTH_KEEP_RELEASES`, and `BERTH_BUILD_ARGS` (the keys `config:set --build-arg` marked). They are not passed to the app; everything else is passed untouched, and `PORT` defaults to 8080.

**Secrets at rest**: v1 relies on the user's login keychain for nothing and on filesystem permissions (0600/0700) for everything, same as Dokku and `~/.ssh`. FileVault, which Macs ship with on, covers the disk unless the user chose `--unattended` mode (see **Reboots and FileVault**). A `--keychain` option that stores `env` in the macOS Keychain is a v2 consideration; it would make headless reboot harder, which is the wrong trade for a closet Mac.

**Reconciliation**: `app.json` and `env` are the source of truth. On every start berthd rebuilds the Caddy config, the cloudflared ingress and the running-container set from them, so a hand edit followed by `berth reload` is a supported path and a corrupted `caddy.json` is never fatal. Reconciliation also removes app containers that no release points at (left by a crash mid-deploy).

## Operations

The Mac should run for months in a closet with nobody logged in at the screen. Five things make that true:

1. **launchd**. `berth init` installs (or, on a re-run, replaces) three LaunchAgents in `~/Library/LaunchAgents`: `dev.berth.berthd`, `dev.berth.caddy`, `dev.berth.cloudflared`, all `KeepAlive` + `RunAtLoad`. Apple's `container` service is per-user, so Berth runs in the user session, not as a system daemon; `berthd` runs `container system start` on launch and retries until it succeeds. Because Berth lives in the user session, a cold boot needs that user logged in; see **Reboots and FileVault** below. Replacing an agent waits for `launchctl bootout` to finish before `bootstrap`: done back to back, the bootstrap fails with "Input/output error" and leaves the agent unloaded, which took the tunnel down on the first real re-run. `berth init` also sets `sudo pmset -a sleep 0 disksleep 0 womp 1 autorestart 1` once, with consent. That and `fdesetup authrestart` for planned reboots (below) are the only `sudo` touches in the product.
2. **Crash recovery**. berthd checks `container list` every 10 s, restarts `container system` if it stopped, and restarts any service container that is down. A container that exits is restarted with exponential backoff (1 s → 60 s cap). Every crash counts, whether or not the restart succeeds, and only 10 healthy minutes reset the count. (The first version counted only failed restarts, so an app that started fine and then crashed again looped forever; the real test caught it.) After 10 crashes the app is marked `crashed` in `berth status`, its route answers 503 at once instead of Caddy retrying dead slots, a `crash.loop` notification goes out, and Berth leaves it alone until the next deploy, rollback or `berth restart`. On berthd start, every app with a `current` release that is not running gets started, oldest first, and Caddy is rebuilt.
3. **Logs**. `container logs --follow` is the source; berthd tails each running container into `~/.berth/apps/<app>/logs/app.log` (rotated at 50 MB, keep 5), prefixing each line with a timestamp and the release (`2026-10-25T04:28:54Z v3: ...`) and remembering how far it got so a berthd restart does not duplicate lines. `berth logs myapp [-f] [-n 200] [--since 1h]`, `--build [n]` for build logs, `--access` for Caddy's JSON access log rendered one line per request, and `berth logs --all` multiplexes every app with name prefixes.
4. **Visibility**. `berth status` is one screen: each app with release, sha, uptime, memory, host port, URL, last deploy result; each service; tunnel connected?; Caddy up?; disk free. `berth status` also lists warnings (crashed apps, services down, low disk, memory over 80%, the FileVault caveat). `berth doctor` checks every assumption (Apple Silicon, macOS version, tools installed, `container` version and system, the `berth` network, launchd agents running, berthd and the webhook listener answering, Caddy admin reachable, tunnel connected, wildcard DNS resolves, Cloudflare and GitHub tokens valid, free disk, sleep settings, reboot mode) and prints a fix for each failure; `--images` compares base images with Docker Hub. `berth events` tails berthd's structured event log (deploys, restarts, webhook deliveries).
5. **Notifications** (shipped in v1). A webhook URL in `config.json` (`berth notify:set https://...`, which sends a test message) receives JSON on deploy success/failure, crash-loop (apps and workers), failed scheduled jobs, and failed nightly and off-Mac backups. The JSON carries `text` (Slack) and `content` (Discord); URLs containing `ntfy` get a plain-text body with a title header instead. A macOS notification on the Mac itself is nearly free to add but nobody is looking at that screen.

**Reboots and FileVault.** macOS disables automatic login while FileVault is on, and a FileVault Mac stops at a pre-boot password screen after any power loss. Berth cannot run until someone types that password, so there is a real trade-off and `berth init` makes the user choose it explicitly:

- **FileVault on (default, recommended for a Mac that holds secrets).** Crashes and restarts of apps, berthd, Caddy or cloudflared all recover unattended. *Planned* reboots (`berth reboot`, used before macOS updates) run `sudo fdesetup authrestart`, which unlocks the disk for the next boot only; the FileVault unlock logs the user in, so the LaunchAgents start with no one at the screen. *Unplanned* power loss stops at the pre-boot password screen until someone types the password there (Screen Sharing cannot reach it). A small UPS is the practical answer. `berth doctor` reports this mode as a warning, not a failure.
- **FileVault off + auto-login (`berth init --unattended`).** Survives power cuts with no human; the disk is unencrypted at rest, so secrets in `~/.berth/secrets` and app `env` files are readable by anyone with physical access. `init` prints the exact System Settings steps (FileVault off, Automatic login on) instead of scripting them, because turning off disk encryption should be a deliberate act by the user.

Which mode is active is recorded in `config.json`, reported by `berth doctor`, and included in `berth status --json`.

**Releases and upgrades.** Releases are git tags `vX.Y.Z`; `scripts/release.sh vX.Y.Z` runs the tests, tags, pushes the tag, and points `Formula/berth.rb` at the tag's tarball with its SHA-256, so `brew install berth` builds the release and `brew upgrade berth` moves to the next (a `--HEAD` install follows `main` and needs `brew upgrade --fetch-HEAD berth`). The formula stamps the version with `-ldflags -X main.version`; a `go install …@vX.Y.Z` build reports the module version from its build info instead.

`berth upgrade` works out how it was installed (a binary under Homebrew's `Cellar/berth/`, `HEAD-` or not; a `go install` build, which has a module version but no VCS stamp; anything else is a source build it can't upgrade), asks GitHub's tags API for the newest `vX.Y.Z`, and runs the matching `brew update` then `brew upgrade berth` (a third-party tap only refreshes on `brew update`: without it, the first real `berth upgrade`, from 0.1.0 to 0.1.1, found nothing to install and said it had succeeded), or `go install`; then it runs the `berth` on `PATH` and fails unless it reports the new version. `--check` only compares. On the Berth Mac it then waits (up to 2 minutes) until berthd answers `ping` with the new binary's version. From a laptop it offers to run `berth upgrade` on the Mac over SSH. berthd notices its binary changed (checked every 10 s), waits until no deploy is running, and re-executes itself in place; running apps are untouched. Caddy, cloudflared and `container` are updated by Homebrew independently; Berth records the `container` version it was tested against in `config.json` and `berth doctor` warns on a different one.

**Off-Mac backups.** The Mac's own backups (nightly dumps, `berth backup`) share its disk; a dead SSD or a stolen Mac takes them too. `berth offsite:setup` points Berth at an S3-compatible bucket (Cloudflare R2 with `--r2 <account id>`, or any `--endpoint`: Backblaze B2, AWS S3, MinIO), checks the credentials by writing, reading, listing and deleting a test object, and stores them in `secrets/offsite.json`. Each night after the 03:00 dumps, berthd streams the same archive `berth backup` writes (minus `backups/offsite/`) straight into the encryptor, so no plaintext copy is written, uploads the result as `<prefix>berth-<UTC time>.tar.gz.enc` (prefix `berth/<mac>/` by default), lists the bucket to check it arrived at the right size, and deletes all but the newest 14 of its own objects. `offsite.json` records the last attempt; a failure is an `offsite.failed` event and notification, and `berth status` warns once nothing has worked for 48 hours. `offsite:get [name|latest]` downloads and decrypts to a `.tar.gz` for `berth restore`.

- *Recovery code*: setup prints, and `offsite:key` repeats, `berthrestore_<base64 JSON>` holding the endpoint, bucket, prefix, credentials and encryption key: the one thing to keep, in a password manager. Anything that takes a key (`--key`) also takes the code.
- *A new Mac* (`berth init --restore`, code asked for hidden or read from `BERTH_RECOVERY_CODE`): after the platform checks and before Cloudflare, init fetches the newest backup and decrypts it into `backups/offsite/`, then `SeedFromArchive` extracts only `secrets/` and `config.json` (merged, keeping this Mac's hostname, gateway, network and ports). The usual steps then find the zone login, the tunnel's credentials and token, and the GitHub token already there, so there's no browser login and the same tunnel is reused; init skips republishing the tunnel's ingress, which would drop custom domains until the apps exist. Once berthd is up, init calls `restore` with the archive and deletes it (it holds every secret in plain form). The old Mac must stop first (`berth uninstall`): two connectors on one tunnel split the traffic.
- *What restore brings back*: everything under `~/.berth` except git mirrors, build directories, logs and live Postgres files: app settings (repo, branch, domains, access policy, links, scale, previews, AI access, dev tokens), config, release history, auth config, groups and sessions, AI settings, service settings with their newest dumps, storage folders, the Caddy CA, and secrets. Restore recreates services from the dumps, relinks (recreating dev databases), and redeploys each app at the commit that was live (its branch if that commit is gone). Dev versions wait for their next `dev:push`; previews are dropped and recreated from their open pull requests; local AI models need `berth ai:setup`; LAN names follow the new Mac's hostname.
- *Results*: a daemon test builds an archive on one Berth and moves it to another the way `init --restore` does: seeding brings the domain, tunnel ID and secrets but no apps and keeps the new hostname; restore redeploys the live commit although the branch moved on, keeps config, access policy and domains, keeps the dev version and drops the preview. With the real R2 backup, a throwaway home was seeded from a recovery code: the domain, the same tunnel (remote-managed), and the GitHub, Google and Cloudflare secrets, with no apps. Not yet run as a whole on a second Mac.

- *Encryption*: XChaCha20-Poly1305 (`x/crypto`) in 64 KiB chunks, each sealed under a 16-byte random per-file prefix plus the chunk counter, with associated data marking the last chunk, so reordering, truncation and tampering fail to decrypt (the STREAM construction). The 32-byte key is made on the Mac at first setup (`secrets/offsite.key`, printed once as `berthkey_…` for the owner to save elsewhere, and again by `offsite:key`); setup never replaces an existing key with a different one, which would orphan earlier backups. The bucket only sees ciphertext and object sizes.
- *Storage client*: Berth's own (no SDK): path-style requests signed with AWS Signature Version 4 (checked against AWS's published example), `UNSIGNED-PAYLOAD` for streamed bodies over HTTPS, multipart upload in 64 MB parts above that size with per-part retries and an abort on failure, and ListObjectsV2 with continuation.
- *Results*: unit tests for the encryption (round trips at chunk boundaries; truncation, a flipped bit and a wrong key all refused), the signer, and the client against a fake S3; a daemon test that sets up, uploads twice with `keep 1`, prunes, lists, downloads and decrypts a valid archive, refuses a wrong key, and warns after a failed upload; and an opt-in test (`BERTH_S3_TEST`) that passed against rclone's S3 server on the test Mac, including a 12 MB multipart upload and a wrong secret being refused, and then against a real Cloudflare R2 bucket: the owner ran `offsite:setup` and `offsite:run` (264 files, 114 KB encrypted, uploaded in 1 s), `offsite:get` downloaded and decrypted a complete archive (Postgres dumps, app settings, config), and the opt-in test passed against R2 too. Not yet run against B2 or AWS S3.

**Resource limits**: default per app 512 MB / 1 CPU, settable with `BERTH_MEMORY`/`BERTH_CPUS`. A 16 GB Mac mini runs \~20 small apps plus Postgres comfortably; `berth status` warns when committed memory exceeds 80% of physical.

## Security model

The attack surface is two HTTP endpoints behind Cloudflare, nothing else. The threat model is "vibe-coded app with a bug" more than "targeted attacker", and the design leans on isolation the user gets for free.

- **No inbound ports.** The tunnel is outbound; the router stays closed. Caddy's LAN listeners (443, and 80 which only redirects to 443) are the only things on the local network, and they only proxy to apps. Service ports are published on the `berth` network's gateway address, which is host-only.
- **Webhook endpoint**: HMAC-SHA256 over the raw body with a per-app 32-byte secret; constant-time compare; delivery-id dedupe; 1 MB body cap; bad signatures limited to 10 a minute per source IP before any work, and only authenticated deliveries count toward an app's 30-a-minute budget, so junk can't crowd out GitHub; unknown apps get the same 401 as a bad signature, so app names can't be probed; only reachable as `hooks.<domain>` through the tunnel. Optionally restrict it at Cloudflare's edge (`berth init --cf-ip-filter`) with a WAF custom rule that blocks `hooks.<domain>` unless the source IP is in GitHub's published `hooks` ranges (from `api.github.com/meta`, refreshed daily by berthd). Cloudflare Access is not used here: it gates on identity, and GitHub cannot log in.
- **berthd control socket** is a unix socket, mode 0600, owned by the user; no TCP admin interface. Caddy's admin API binds `127.0.0.1` only.
- **Per-app isolation** is Apple's: each app is its own micro-VM with its own kernel, so a container escape in one app does not reach another app, the Postgres VM or macOS. This is materially stronger than Dokku on a shared Docker daemon and is the best argument for `container` over OrbStack.
- **Containers see only what they are given**: their env snapshot, their mounts, the `berth` network. No access to the user's home directory, the GitHub token, or other apps' env. Builds run in `container build`'s builder VM, not on the host.
- **Least privilege for tokens**: the GitHub token comes from Berth's OAuth app with scopes `repo` + `admin:repo_hook`, and can be revoked at github.com/settings/applications. Cloudflare credentials are never account-wide API keys: the tunnel credential (`secrets/cloudflared/<tunnel>.json`) can only run the tunnel; each zone's login cert (`secrets/cloudflared/zones/<zone>.pem`) carries a token limited to tunnels and that zone's DNS; and the optional scoped token in `secrets/cloudflare.token` exists only for `--cf-ip-filter`.
- **Secrets**: never logged (debug logs redact `-e` values), masked in `config:show` (by name, and by value for URLs with a password and PEM blocks), never in build args unless the user opts in with `berth config:set --build-arg`; env snapshots, service env files and `config.json` are 0600 under a 0700 `~/.berth`. Service passwords travel by `--env-file`, and role passwords reach `psql`/`redis-cli` over stdin, never argv. The one exception: an app env value containing a newline (a PEM key, say) must be passed to `container run` as `-e`, because `--env-file` can't carry newlines, so it is briefly visible in the process list while the container starts.
- **Input validation**: values that reach git, shell hooks, Caddy matchers or Cloudflare expressions are validated where they enter: branches, paths and refs follow git ref rules and can't start with `-`; repos can't start with `-` or contain shell metacharacters; custom domains must be plain hostnames (no wildcards) and can't collide with `hooks.<domain>`, another app's `<app>.<domain>` or LAN names; the `git push berth` hook single-quotes every field anyway.
- **Supply chain**: images are pulled from the registry the Dockerfile names; Berth templates pin major tags (`node:22-slim`, `postgres:17`). `berth doctor --images` lists images with available updates.
- **Sign-in**: Google login runs in berthd behind Caddy's `forward_auth` (see Sign-in). Clients can't forge identity headers (Caddy deletes `X-Berth-*` on every route); one-time codes, nonce cookies and PKCE protect the login hand-off; redirects only go to Berth app hostnames and same-host paths; logout is POST with an Origin check; sessions are stored hashed. If berthd is down, apps with a policy answer 502, so `allow` and `signed-in` apps fail closed.
- **Working from another machine** adds no listener: commands travel over the user's own SSH, as the Mac's user, with every argument single-quoted.
- **Pull request previews** run only branches of the app's own repository, never forks, behind the dev versions' access rule; see Pull request previews.
- **Off-Mac backups** are encrypted on the Mac with a key the provider never sees; the bucket credentials need only object read, write, list and delete on that bucket.
- **Admin page**: only `@owners`, checked twice (Caddy's sign-in check, then berthd verifying the signed identity for the admin hostname); read-only; strict CSP; never shows env values.
- **AI gateway**: provider keys never leave berthd; apps get per-app tokens (stored hashed) that only the gateway accepts, limited by model, tokens a month and requests a minute. The gateway port answers only the container network and loopback. Prompts aren't logged.
- **Public by default is a choice**: `berth create --private` creates the app with no public hostname: its Caddy route matches only LAN hosts, so tunnel requests for it get a 404; `berth auth:set` adds basic auth in Caddy; both are one flag because the hobbyist default should be safe to flip either way.

## CLI reference

Dokku's `noun:verb` style, with the handful of commands people type daily at the top level. Every non-interactive command accepts `--json` for agents and scripts (progress goes to stderr, the result to stdout). Column 3 is the Dokku equivalent, for muscle memory.

| Command | Does | Dokku |
| --- | --- | --- |
| `berth init --domain d [--no-tunnel] [--cf-ip-filter] [--unattended] [--github-token t] [--cf-token t] [--no-launchd] [--yes]` | one-time setup of tunnel, Caddy, launchd, GitHub login | bootstrap.sh |
| `berth new <app> [--stack node\|python\|go\|static] [--repo r] [--public] [--no-github] [--no-deploy] [-y]` | a starter app with `AGENTS.md`, a GitHub repo through `gh`, then `create` | — |
| `berth create <app> [--repo owner/name\|url\|path] [--branch b] [--path p] [--private] [--no-webhook] [--no-deploy]` | register app, webhook, hostname; first deploy | `apps:create` + git remote |
| `berth deploy <app> [--ref sha] [--no-cache]` | build + release now | `git push dokku` |
| `berth rollback <app> [n] [--with-data] [--yes]` | swap to previous (or release n); `--with-data` also restores the databases from before the later release's command ran | `ps:rebuild` / manual |
| `berth logs <app>\|--all [-f] [-n N] [--since 1h] [--build [n]] [--release [n]] [--access] [--cron]` | app, build, release-command, access or scheduled-job logs | `logs` |
| `berth status [app]` | everything at a glance | `ps:report` |
| `berth restart\|stop\|start <app>` | lifecycle (restart is zero-downtime; a stopped app answers 503) | `ps:restart` etc. |
| `berth destroy <app> [--keep-data] [--yes]` | remove app, hostname, webhook, images, link roles | `apps:destroy` |
| `berth config:set\|get\|unset\|show\|import <app> ...` | env vars | `config:*` |
| `berth domains:add\|remove\|list <app> <host>` | extra hostnames via tunnel ingress | `domains:*` |
| `berth auth:set\|unset <app> user:pass` | basic auth in Caddy | http-auth plugin |
| `berth access <app> [path] <public\|signed-in\|allow who...\|off> [--remove]` | Google sign-in policy for an app or path | — |
| `berth dev:push [app] [--json]` / `dev:token <app> [--as email] [--revoke]` / `dev:db:copy <app>` / `dev:reset <app>` / `dev:agents <app>` / `dev:off\|on <app>` | dev versions at `<app>-dev.<domain>` (any app command also works on `<app>-dev`) | — |
| `berth previews <app>` / `previews:on <app> [--max n]` / `previews:off <app>` | an app per open pull request, `<app>-pr-<n>` | review apps |
| `berth ps:scale <app> <type>=<n>...` | worker processes per Procfile process type | `ps:scale` |
| `berth cron <app>` / `cron:run <app> <n>` | scheduled jobs from `app.json`: list, run one now | `cron:list` / `cron:run` |
| `berth admin` / `admin:enable [--host h] [--owner email]` / `admin:disable` | the admin page at `admin.<domain>`, for `@owners`: status, analytics, live requests, logs, events | — |
| `berth ai:setup [--model m] [--cloud-only]` / `ai:key <provider> [key] [--url u]` / `ai:alias <name> <provider/model>` / `ai:pull <model>` / `ai:enable <app> [--models …] [--monthly-tokens n] [--rpm n]` / `ai:disable <app>` / `ai:status` / `ai:models` / `ai:usage [app]` | the AI gateway: local models with Ollama, cloud providers with Berth-held keys | — |
| `berth auth:setup google [--host h] [--owner email]` / `auth:status` / `auth:group <name> [who...]` / `auth:sessions` / `auth:revoke <email>` | sign-in setup, groups and sessions | — |
| `berth storage:mount\|unmount\|list <app> <path>` | persistent folders | `storage:*` |
| `berth pg:create\|link\|unlink\|psql\|backup\|restore\|expose\|upgrade\|destroy\|list` | Postgres service | dokku-postgres |
| `berth redis:*` | Redis service (same verbs; `redis:cli` instead of `pg:psql`) | dokku-redis |
| `berth sqlite:add\|remove\|backup\|restore\|shell\|list` | an app's own SQLite database, on a volume one container at a time can open | — |
| `berth releases <app>` | list releases with sha, status, time | `ps:report` |
| `berth run <app> <cmd>` | one-off command in a fresh container with the app's env (migrations); inside the running container for an app with SQLite | `run` |
| `berth exec <app> <cmd>` | shell into the running container | `enter` |
| `berth dockerfile <app>` | print the template Berth would use | — |
| `berth git:remote <app> [--host h]` | add a `berth` SSH remote for push-to-deploy, `dev:push` and running commands from another machine | built in |
| `berth app:set-key <app>` | fetch over SSH with a generated deploy key | `git:*` |
| `berth app:set-repo <app> <repo> [--branch b]` | change an app's source; keeps config, links and releases, moves the webhook, records the live sha so nothing redeploys | — |
| `berth doctor` / `berth events` / `berth gc` / `berth backup` / `berth restore` | ops | — |
| `berth reboot` | planned reboot that comes back unattended (`fdesetup authrestart`) | — |
| `berth reload` | re-apply `~/.berth` to containers, Caddy and the tunnel after hand edits | — |
| `berth cf:login <zone>` | connect another Cloudflare zone by browser login, for `domains:add` | — |
| `berth trust-ca` | trust Caddy's local CA in the login keychain | — |
| `berth uninstall [--purge]` | remove the launchd agents and stop everything (`--purge` also deletes apps, volumes, images and `~/.berth`) | — |
| `berth notify:set <url\|off>` | deploy/crash notifications | — |
| `berth offsite:setup [--r2 id\|--endpoint u] --bucket b [--key k]` / `offsite:status` / `offsite:run` / `offsite:get [backup]` / `offsite:key` / `offsite:off` | encrypted nightly backups to S3-compatible storage | — |
| `berth upgrade [--check] [-y]` | upgrade Berth as it was installed; from a laptop, the Mac too | `dokku-update` |

On a machine without Berth, every command except `new`, `dev:push`, `git:remote`, `admin`, `upgrade` and setup runs on the Mac over SSH (`$BERTH_HOST`, or the repo's `berth` remote).

Output conventions: success ends with the URL or the next command; errors are one line plus `berth doctor` or `berth logs` as the pointer; long operations stream (`building ... ✓ 23 s`). Shell completions (`berth completion zsh|bash|fish`) ship with the brew formula.

## Implementation plan for a coding agent

Go 1.23+, one module, two binaries from one `cmd/`. Shell out to `container`, `git`, `cloudflared`; talk to Caddy over HTTP. No CGO, no database, no framework beyond `cobra` for the CLI (other dependencies: `golang.org/x/crypto/bcrypt` for basic auth, `golang.org/x/term`). Target was \~6k lines including tests; v1 came to about 10.5k plus 1.1k of tests, and with sign-in, dev versions, previews, the AI gateway, the admin page, workers, cron and off-Mac backups it is about 22k plus 5k (Oct 2026). Dependencies added since: `golang.org/x/crypto/chacha20poly1305` for off-Mac backups. Each milestone ends with a demo the agent can run end-to-end on a real Mac mini; do them in order and do not start the next until the demo passes.

**Repo layout**

```
berth/
  cmd/berth/main.go          # CLI (cobra); every command = one file in internal/cli
  cmd/berthd/main.go         # daemon entry
  internal/
    state/      # config, app.json, releases, env, services, events: load/save, atomic writes
    runtime/    # wrapper over `container` CLI: build, run, stop, logs, list, stats, images, volumes, network (interface + fake for tests)
    caddy/      # full-config generation from state + admin API client (/load)
    tunnel/     # cloudflared ingress/config, cloudflared CLI for init, Cloudflare API (tunnel config, DNS, WAF)
    github/     # device-flow login, hooks API, branch-head polling, HMAC verify
    gitx/       # bare mirror, token-in-env fetch, export a commit's tree
    rpc/        # CLI ↔ berthd protocol (NDJSON over the unix socket)
    deploy/     # the pipeline: fetch → dockerfile → build → run → health → swap → gc
    detect/     # stack detection, renders templates/*.Dockerfile
    services/   # Postgres and Redis: create, link roles, backup/restore, expose, upgrade
    daemon/     # RPC handlers, webhook server, supervisor, log tails, poller, scheduler, status, backup/restore, mDNS
    launchd/    # plist generation + launchctl bootstrap/bootout/kickstart
    doctor/     # berth doctor's checks
    cli/        # one file per command group
  templates/    # Dockerfiles and the launchd plist (Go package embedding them)
  design.md     # this doc
  Formula/berth.rb
  Makefile      # build, test (go test -race ./...)
```

**Milestones**

1. **Skeleton + runtime wrapper** — `berth doctor` passes on a clean Mac; `runtime` can build a hello-world Dockerfile and run it on a port; fake runtime for CI on Linux.
2. **Deploy pipeline, local only** — `berth create hello --repo ... --no-webhook` then `berth deploy hello` serves on `127.0.0.1:<port>`; releases, rollback, logs, config:set work. Demo: deploy, change an env var, rollback.
3. **Caddy** — routes per app on `.local` with internal TLS; zero-downtime swap verified by a `hey`/`oha` run during a deploy showing 0 failed requests.
4. **Tunnel + DNS** — `berth init --domain` end to end; `https://hello.<domain>` works from a phone on cellular.
5. **GitHub** — device-flow login, webhook registration, HMAC verify, polling fallback; demo: `git push` → live in under 60 s, plus a redelivery is a no-op.
6. **Postgres** — `pg:create/link/psql/backup/restore`; demo: a Node app with Prisma migrates and serves from it; nightly dump fires.
7. **launchd + supervisor** — reboot the Mac; everything comes back without a login prompt touched; kill an app's process; it restarts.
8. **Polish** — storage:mount, domains:add, auth:set, gc, backup/restore, `--json` everywhere, completions, brew formula, README with a 90-second GIF.

**Milestone status (v1)**: 1, 2, 3, 6 and 8 are done and demoed on a real Mac (the Postgres demo used a Flask app rather than Prisma; nightly dumps are implemented but were not observed firing at 03:00). 5 is done except against real GitHub: HMAC, redelivery, branch filtering, polling and push-to-live were demoed with signed local requests and a local repo; webhook registration and the API calls are tested against mock servers. 7's supervisor half is done (crash restart in 8 s, berthd self-restart on upgrade); the launchd half is untested. 4 is implemented but untested without a Cloudflare account: `cloudflared tunnel ingress validate` accepts the generated config and routes each hostname as designed, and the API calls are tested against a mock. The README GIF is not made. *Since then* (Oct 2026), 4, 5 and the launchd half of 7 have run for real on cronbon.com: the tunnel and webhooks carry daily traffic and launchd keeps berthd, Caddy and cloudflared running; nightly dumps, a power cut and `berth reboot` are still unobserved.

**Acceptance tests** (checked, not aspirational): the three-command install on a fresh macOS 26 account completes in < 5 min with no `sudo` beyond the two consented commands; push-to-live p50 < 45 s for a Node app with a warm cache; zero failed requests across 20 consecutive deploys under load; power-cycle recovery < 2 min with all apps and Postgres healthy (in `--unattended` mode, or measured from the FileVault unlock otherwise); `berth restore` on a second Mac reproduces every app and database; `go test ./...` runs on Linux with the fake runtime and covers state, deploy ordering, HMAC, detection and Caddy config generation.

Results so far: zero failed requests across 20 deploys and a restart through the real Cloudflare tunnel at 20 requests/s (1,325 requests, p50 28 ms), and locally across 20 deploys at 100 requests/s (5,071 requests); push-to-live through a real GitHub webhook about 8 s for the hello-world FastAPI app, and 12 s with `git push berth` over SSH; crash-loop give-up after 10 crashes with the notification delivered through ntfy.sh; push-to-live about 3 s for a small Node app with a warm cache; `berth restore` after a full purge reproduced every app, database, Redis key, volume and config var in 15 s on the same Mac; `go test -race ./...` passes and every package compiles for Linux. The install, power-cycle and second-Mac tests have not been run. Google sign-in worked end to end on the first real install: `auth:setup google`, then sign-in on `auth.cronbon.com` and a one-redirect hand-off to Chirp (`berth access chirp public`), whose posts now carry the Google identity. Dev versions on the same install: an uncommitted change in a Chirp checkout was live at `chirp-dev.cronbon.com` 5.6 s after `berth dev:push` (2.4 s build), with a token user posting to its own `chirpdb_dev`; the dev login is refused by production's database and vice versa; broken builds and crashing starts exit 1 and 2 with the previous dev release still serving; the same push over SSH from a "remote" checkout returned the JSON result.

**Guidance for the agent**: read Apple's `container` docs (`github.com/apple/container/docs`) for the exact flags before writing `runtime/`; prefer one shell-out with `--format json` over parsing text; make every write to `~/.berth` atomic (write temp, rename); log every external command at debug level with its duration; keep each CLI command under 150 lines; when in doubt, copy the Dokku behaviour and name.

## Open questions and known limitations

Things to settle in milestone 1 by trying them, and limits worth stating in the README. Answers from building v1 on macOS 26.1 with `container` 1.6.1 are recorded inline.

- [x] **Volume semantics in `container`**: a named volume is an ext4 disk image in `~/Library/Application Support/com.apple.container/volumes/` and cannot be pinned elsewhere. Bind-mounting a host folder works for app storage but not for Postgres (its entrypoint's `chown` fails on virtiofs). Postgres uses named volumes; Time Machine sees the nightly dumps, not the live data. A named volume can be attached to one running VM at a time (a second gets `The storage device attachment is invalid`), which is what makes it the right home for SQLite; and SQLite's locks don't cross VMs on a shared bind mount (see **SQLite**).
- [x] **Build cache**: yes. The BuildKit builder VM keeps its layer cache across builds (a one-line change rebuilt in 0.6 s). Measured push-to-live with a warm cache: about 3 s for a small Node app.
- [x] **DNS domain per network**: no. Container-to-container name resolution needs `sudo container system dns create <domain>`. Berth publishes services on the network gateway instead (see Networking), which keeps setup free of `sudo`.
- [x] **Idle memory**: measured by `container stats`: about 35 MB for a minimal Python server, 60 MB for Caddy serving a static site, 105 MB for gunicorn + Flask, 130 MB for Node 22, 36 MB for Redis and 210 MB for Postgres 18. Node misses the 60 MB target; the 20-apps-on-16-GB estimate still holds with room to spare.
- [ ] **launchd + `container system`**: confirm the service starts cleanly at login with no GUI session interaction, and confirm that `fdesetup authrestart` brings the user session (and so the LaunchAgents) back without a login prompt on macOS 26. *Half verified (Oct 2026): on the test Mac the agents load at login and keep berthd, Caddy and cloudflared running (`launchctl kickstart -k` restarts berthd cleanly). Whether `fdesetup authrestart` brings the session back without a login prompt is still untested.*
- [x] **Container network loss**: twice in two hours of testing, every container lost outbound internet (`Network is unreachable`, builds stuck on `pip install`, apps unable to call external APIs) while the Mac itself was fine and inbound traffic still worked. `container system stop && container system start` fixes it, and costs about 6 s of downtime while berthd brings the services and apps back. berthd now probes outbound access every 5 minutes from a throwaway Alpine container (every minute once a probe fails). After two failures in a row, with no deploy in flight, it restarts the container system itself and sends a `network` notification; this doesn't count as a crash. Set `auto_heal_network: false` in `config.json` to get only the notification. Builds that fail with network-error signatures say so, and `berth doctor` runs the same probe.
- [ ] **Rosetta**: should `berth` pass `--rosetta`-style flags for amd64-only images automatically, or just document "use arm64 images"? Default: detect `linux/amd64`-only manifests and warn. *Not implemented in v1; the builder VM has Rosetta enabled by default (`container system property list`), so amd64 base images build.*
- [x] **Name**: *Berth*, at github.com/dps/berth.
- [ ] **GitHub device flow**: needs Berth's GitHub OAuth app (device flow enabled; its public client ID is compiled in as `github.DefaultClientID`, overridable with `BERTH_GITHUB_CLIENT_ID`).
- [x] **Cloudflare login-cert token permissions**: verified on cronbon.com: the token in `cloudflared tunnel login`'s cert can read the zone and its DNS records, delete records, and read and write the tunnel's remote configuration. DNS records are created with `cloudflared tunnel route dns`.

**Known limitations, v1**

- With FileVault on, an unplanned power loss needs a person to unlock the Mac before apps come back; a UPS or `--unattended` mode avoids it.
- Apple Silicon + macOS 26 only; no Intel, no Linux. (The abstraction in `runtime/` would admit a Docker backend later, but that is not a goal.)
- One process type per app; no cron scheduler; no multi-host.
- Cloudflare account required for public URLs (free tier is fine). LAN-only mode exists for people who do not want Cloudflare.
- A home connection's upload speed is the ceiling; Berth is for apps, not for serving large media.
- Postgres, Redis and SQLite only; other services (Mongo, MySQL, MinIO) are either a Dockerfile in your own app or a v2 plugin API, deliberately deferred until two people ask.
- An app with SQLite deploys with a few seconds' wait for visitors (more if its release command is slow), and is backed up from the file at `SQLITE_PATH` only; a database the app keeps under another name on the volume persists but isn't backed up. Whether a SQLite commit inside a VM is durable on the Mac's disk when it returns is unverified (the same holds for Postgres).

**Sources**: [apple/container](https://github.com/apple/container) · [container CLI `run` reference](https://mintlify.com/apple/container/cli/run) · [container networking notes](https://instagit.com/apple/container/how-to-network-multiple-containers/) · [Dokku docs](https://dokku.com/docs/) · [Cloudflare Tunnel docs](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) · [Caddy admin API](https://caddyserver.com/docs/api)
