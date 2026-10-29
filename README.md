<p align="center"><img src="docs/icon.svg" alt="Berth: a bollard with a rope tied into a B" width="160"></p>

<h1 align="center">Berth</h1>

**A dead simple PaaS for Mac minis.** You push to GitHub, and a few seconds
later your app is live at `https://myapp.yourdomain.com`, running in its own
little VM on a Mac in your closet. Postgres is one command away.

```bash
brew tap dps/berth https://github.com/dps/berth
brew trust dps/berth                      # Homebrew 7+ wants you to OK third-party taps
brew install berth

berth init --domain example.com           # once
berth create myapp --repo you/myapp       # once per app
git push                                  # every deploy after that
```

No ports opened on your router, no certificates, no YAML, no Docker Desktop,
and no `sudo` once it's set up. Starting from an empty folder?
`berth new myapp` makes a starter app with instructions for your coding agent,
creates the GitHub repo and deploys it.

## Why

I wanted the old Heroku feeling (push, and it's live) for the pile of small
apps I and my coding agents keep making, without a monthly bill per app or a
weekend lost to Kubernetes. A Mac mini turns out to be a great home for that.
It's quiet, it sips power, and an M4 with 16 GB comfortably runs twenty-odd
small apps and a couple of databases. Apple's
[`container`](https://github.com/apple/container) runtime gives each app its
own lightweight Linux VM, and a Cloudflare tunnel puts it on the internet
without opening anything at home.

Berth is the glue. Each piece of it exists because something was annoying
without it: zero-downtime deploys, nightly database dumps, a dev version an
agent can push to and test on its own, sign-in so I stop writing OAuth code,
and an encrypted backup somewhere that isn't the Mac.

### What it isn't

It's one Mac. There's no failover, and if the power goes out while you're on
holiday your apps are down until it comes back (and if FileVault is on,
until someone types the password: see [Reboots and FileVault](#reboots-and-filevault)).
That's the right trade for side projects, internal tools, family apps and
prototypes. It's the wrong one for anything where an hour of downtime costs
real money.

---

## Contents

**Getting going:**
[What you need](#what-you-need) ·
[Setting up](#setting-up) ·
[Starting a new app](#starting-a-new-app) ·
[Deploying an app](#deploying-an-app)

**Day to day:**
[Releases and rollbacks](#releases-and-rollbacks) ·
[Config](#config) ·
[Logs and status](#logs-and-status) ·
[One-off commands](#one-off-commands)

**Data and background work:**
[Postgres and Redis](#postgres-and-redis) ·
[SQLite](#sqlite) ·
[Migrations](#migrations) ·
[Workers and scheduled jobs](#workers-and-scheduled-jobs) ·
[Persistent storage](#persistent-storage)

**Who sees what:**
[Domains, LAN URLs and passwords](#domains-lan-urls-and-passwords) ·
[Sign in with Google](#sign-in-with-google)

**Working with agents:**
[Dev versions](#dev-versions) ·
[Pull request previews](#pull-request-previews) ·
[AI for your apps](#ai-for-your-apps)

**Running the Mac:**
[The admin page](#the-admin-page) ·
[Living in a closet](#living-in-a-closet) ·
[Working from your laptop](#working-from-your-laptop) ·
[Backups and moving to a new Mac](#backups-and-moving-to-a-new-mac) ·
[Upgrading](#upgrading)

**Reference:**
[Commands](#commands) ·
[Troubleshooting](#troubleshooting) ·
[Example apps](#example-apps) ·
[How it works](#how-it-works) ·
[Hacking on Berth](#hacking-on-berth)

---

## What you need

- **An Apple Silicon Mac on macOS 26 or later.** A Mac mini is ideal.
- **A domain on Cloudflare** for public URLs (the free plan is fine). Without
  one, Berth still works on your LAN.
- **A GitHub account.** Berth asks you to authorize it in the browser so it
  can clone private repos and add webhooks. Public repos work without it;
  Berth polls them instead.

Homebrew installs everything else: Apple's `container` runtime,
[Caddy](https://caddyserver.com) and
[cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/).

## Setting up

```bash
brew tap dps/berth https://github.com/dps/berth
brew trust dps/berth
brew install berth
berth init --domain example.com
```

`berth init` takes about three minutes. Here's what it does:

1. **Checks the Mac** and installs anything missing (`container`, `caddy`,
   `cloudflared`).
2. **Starts Apple's container system.** The first time, it downloads a small
   Linux kernel.
3. **Logs in to Cloudflare** in your browser. Pick `example.com` when it asks
   which zone. Berth makes a tunnel called `berth` and points `*.example.com`
   at it, so every app you create later gets its hostname without touching
   DNS. No API token needed.
4. **Logs in to GitHub** in your browser. The page wants a one-time code,
   which Berth has already put on your clipboard: paste it and click
   *Authorize*. (No browser? Berth uses your logged-in `gh` CLI, or asks for a
   classic token with the `repo` and `admin:repo_hook` scopes.)
5. **Installs three background services** (launchd agents): `berthd`, Caddy
   and cloudflared. They start at login and restart if they die.
6. **Asks before the two things that need `sudo`**: keeping the Mac awake and
   restarting after a power cut (`pmset`), and trusting Caddy's local
   certificate authority so `https://*.local` gets a padlock in Safari.
7. **Explains the FileVault trade-off** ([more below](#reboots-and-filevault))
   and runs a health check.

You can run `init` again any time. It keeps your settings and logins and only
changes what the flags say.

Some variations:

| You want | Run |
| --- | --- |
| LAN only, no Cloudflare | `berth init --no-tunnel` |
| No browser at all | `berth init --domain example.com --github-token ghp_… --yes` (Cloudflare still needs one browser login, or a copied `~/.berth/secrets/cloudflared/`) |
| Only GitHub's servers can reach the webhook URL | `berth init --cf-ip-filter --cf-token …` (the one thing that needs a Cloudflare API token, with *Zone · WAF · Edit*) |
| Come back from power cuts with nobody home | `berth init --unattended` ([read this first](#reboots-and-filevault)) |
| Public repos only, no GitHub login | `berth init --skip-github` |
| Set up a replacement Mac from your backups | `berth init --restore` ([how](#moving-to-a-new-mac)) |

`--yes` never runs `sudo`. Do the power settings yourself with
`sudo pmset -a sleep 0 disksleep 0 womp 1 autorestart 1`.

## Starting a new app

```bash
berth new myapp                    # or --stack python | go | static
```

You get a folder called `myapp` that's ready to hand to a coding agent:

- **A small web app** that already follows [the rules](#what-your-app-must-do):
  Node with no dependencies (the default), Python with FastAPI, Go with the
  standard library, or a static site.
- **`AGENTS.md`**, which Codex, Cursor and most other agents read. It tells
  the agent how to try changes on the [dev version](#dev-versions) with
  `berth dev:push`, read logs, set config, add a database, write migrations,
  add workers and cron jobs, use [sign-in](#sign-in-with-google) and
  [AI](#ai-for-your-apps) if your Berth has them, and which things to ask you
  about first (pushing to `main`, rollbacks, production config). It has your
  app's real URLs in it. It's yours: edit away.
- **`CLAUDE.md`**, one line pointing Claude Code at `AGENTS.md`.
- A README, a `.gitignore` and a first commit on `main`.

Then it asks about two more things:

1. **GitHub.** If the [GitHub CLI](https://cli.github.com) is logged in
   (`brew install gh && gh auth login`), Berth makes a private repo,
   `<you>/myapp`, and pushes to it. `--public` makes it public; `--repo` picks
   another name or an organization. Without `gh`, it prints the three steps to
   do by hand.
2. **Deploying.** It creates the app from that repo and deploys it, just like
   [`berth create`](#deploying-an-app).

`-y` says yes to both, and `--no-github` and `--no-deploy` skip them. Then
open the folder in your agent and ask for what you want.

`berth new` works on your laptop too, which is probably where your editor and
agent live: see [Working from your laptop](#working-from-your-laptop). For an
existing repo, `berth dev:agents myapp` prints the Berth section of
`AGENTS.md` for you to paste in.

## Deploying an app

```bash
berth create myapp --repo you/myapp
```

This adds a webhook to the repo, gives the app its hostnames and deploys
`main`. From then on, every push to `main` deploys.

```
-----> Registered webhook on github.com/you/myapp
-----> https://myapp.example.com
-----> https://myapp.your-mac.local
-----> Fetching you/myapp (main)
-----> Building myapp v1 from main@a1b2c3d (detected node)
-----> Built berth/myapp:1 in 21s
-----> Starting berth-myapp-1-x7k2 on 127.0.0.1:8001
-----> Healthy in 1.0s (HTTP 200)

myapp is live at https://myapp.example.com
```

### What your app must do

The same four rules as Heroku and Dokku, so anything written for them (or by
an agent that knows them) just works:

1. Listen on `$PORT` (8080 unless you change it) on `0.0.0.0`.
2. Read its configuration from environment variables.
3. Only write to `/tmp` or a [mounted folder](#persistent-storage). Everything
   else is wiped on each deploy.
4. Log to stdout and stderr.

### Dockerfiles (or not)

If there's a `Dockerfile` (or `Containerfile`) at the root of the repo, Berth
builds it. If it's somewhere else:
`berth config:set myapp BERTH_DOCKERFILE=deploy/Dockerfile`.

Otherwise Berth works out the stack and writes one for you:

| Found in the repo | Stack | Starts with |
| --- | --- | --- |
| `bun.lock` / `bun.lockb` | Bun | `bun run start`, or your entry file |
| `package.json` | Node (npm, pnpm or yarn by lockfile; version from `engines.node`) | the `start` script, or `server.js` / `index.js` |
| `pyproject.toml` / `requirements.txt` | Python, installed with uv | uvicorn, gunicorn or `flask run` for FastAPI/Flask/Starlette; gunicorn for Django; else `python main.py` |
| `go.mod` | Go (multi-stage, small Alpine image) | the binary from `.` or `./cmd/<name>` |
| `Cargo.toml` | Rust (multi-stage) | the package's binary |
| `index.html` | Static site, served by Caddy | — |

A `build` script in `package.json` runs during the build. To see what Berth
generates, or take it over:

```bash
berth dockerfile myapp > Dockerfile    # commit it and edit as you like
```

### Branches, staging and monorepos

```bash
berth create myapp-staging --repo you/myapp --branch staging     # another app for another branch
berth create api --repo you/mono --path services/api             # build from a subfolder
```

A monorepo app only deploys when a push touches files under its path.

To point an existing app at a different repo or branch (say you started from a
local folder and have now pushed it to GitHub):

```bash
berth app:set-repo myapp you/myapp [--branch main]
```

Config, databases, sign-in and release history all stay put. Berth moves the
webhook over and only redeploys if the branch is ahead of what's live.

### How deploys get triggered

| Trigger | When |
| --- | --- |
| **GitHub webhook** | Pushes deploy within seconds. `berth create` sets it up if you have a tunnel and a GitHub login. |
| **Polling** | Every 60 seconds Berth checks the branch. Always on, so it catches missed webhooks, `--no-webhook`, `--no-tunnel` and repos that aren't on GitHub. |
| **`git push berth main`** | Push straight to the Mac over SSH, Dokku-style, and watch the build in your terminal. |
| **`berth deploy myapp`** | Deploy right now. `--ref <sha|branch|tag>` for a particular commit, `--no-cache` for a clean build. |

`--repo` also takes any git URL (`https://gitlab.com/you/app.git`) or a local
path. Or leave it out for an app you only ever push to:

```bash
berth create myapp                 # no repo
cd ~/code/myapp && berth git:remote myapp
git push berth main
```

`git push berth` needs **Remote Login** turned on on the Mac (System
Settings → General → Sharing).

### Private repos

The GitHub login from `init` covers private repos. If you'd rather use a
read-only SSH deploy key for a particular repo:

```bash
berth app:set-key myapp     # makes a key and adds it to the repo (or prints it for you to add)
```

## Releases and rollbacks

Every deploy makes a numbered **release**: an image plus the config it
started with.

```bash
berth releases myapp
```
```
  REL  SHA      STATUS   TRIGGER  WHEN     MESSAGE
* v4   b3ab439  running  webhook  2m ago   Add pricing page
  v3   9f1c2de  stopped  config   1h ago   Fix login redirect
  v2   9f1c2de  failed   webhook  1h ago   health check / failed after 1m0s: HTTP 502
  v1   4e5a6b7  stopped  create   2d ago   First commit
```

Deploys have no downtime. The new release starts next to the old one, and
Berth waits for it to answer on `/` (anything below a 500 counts) before
moving traffic over. The old one gets 5 seconds to finish its requests, then
stops. If the new release never gets healthy, the old one just keeps serving,
and you get the new one's last 50 log lines so you can see why.

```bash
berth rollback myapp          # back to the previous good release, in about 2 seconds
berth rollback myapp 2        # back to v2
berth restart myapp           # fresh container, same release, no downtime
berth stop myapp              # the URL answers 503 until you start it
berth start myapp
```

A rollback brings back that release's code *and* its config, but not its
data. If a migration changed the database, see
[rolling back data](#rolling-back-data). Berth keeps images for the last 5
releases; older ones are cleaned up, and you can still redeploy them by
commit with `berth deploy myapp --ref <sha>`.

## Config

Config is environment variables. Changing them makes a new release from the
current image (no rebuild), so it's live in a few seconds.

```bash
berth config:set myapp OPENAI_API_KEY=sk-… GREETING=hello
berth config:unset myapp GREETING
berth config:get myapp OPENAI_API_KEY
berth config:show myapp                 # secrets masked; --reveal to show them, --shell for KEY=value
berth config:import myapp .env          # add a .env file; --replace drops everything else
berth config:set myapp FOO=bar --no-restart     # save it now, use it on the next deploy
```

`config:show` hides anything ending in `_KEY`, `_SECRET`, `_TOKEN` or
`PASSWORD`, plus `DATABASE_URL` and `REDIS_URL`.

Your app gets its variables when it runs, not when it builds. If the build
really does need one (a public API URL baked into a frontend, say), mark it:

```bash
berth config:set myapp NEXT_PUBLIC_API_URL=https://api.example.com --build-arg
```

### Settings for Berth itself

Variables starting with `BERTH_` tune how Berth runs the app. Your app never
sees them.

| Variable | Default | What it does |
| --- | --- | --- |
| `BERTH_HEALTHCHECK` | `/` | the path checked before traffic moves to a new release |
| `BERTH_HEALTH_TIMEOUT` | `60` | seconds to wait for it to get healthy |
| `BERTH_DRAIN` | `5` | seconds the old release keeps running after the switch |
| `BERTH_MEMORY` | `512M` | memory limit (`256M`, `1G`, …) |
| `BERTH_CPUS` | `1` | CPU limit |
| `BERTH_KEEP_RELEASES` | `5` | release images kept for rollback |
| `BERTH_DOCKERFILE` | — | Dockerfile path in the repo (or the `--path` subfolder) |
| `BERTH_RELEASE_COMMAND` | Procfile `release:` | command run before each release, usually migrations ([more](#migrations)) |
| `BERTH_RELEASE_TIMEOUT` | `600` | seconds the release command gets |
| `BERTH_RELEASE_BACKUP` | `on` | `off` skips the Postgres backup before the release command |
| `BERTH_CRON_TIMEOUT` | `3600` | seconds a [scheduled job](#scheduled-jobs) gets |
| `PORT` | `8080` | the port your app listens on inside its container |

## Logs and status

```bash
berth logs myapp                   # the last 200 lines
berth logs myapp -f                # follow
berth logs myapp --since 30m -n 1000
berth logs myapp --build           # the latest build (or --build 12)
berth logs myapp --release         # the latest release command, i.e. migrations
berth logs myapp --access          # a line per HTTP request
berth logs myapp --cron            # scheduled jobs
berth logs --all -f                # every app at once
```

Every line has the time and the release that wrote it. Logs live in
`~/.berth/apps/<app>/logs/`, outlive the containers that wrote them, and roll
over at 50 MB.

`berth status` is the one screen I look at most:

```
berth v0.1.2 · example.com · berthd up 3d4h
container ✓ running · caddy ✓ up · tunnel ✓ connected (remote) · disk 101.7 GB free · memory 2.2 GB of 16.0 GB committed

APP     STATE    REL  SHA      UPTIME  MEM            PORT  URL                        LAST DEPLOY
myapp   running  v4   b3ab439  2h      131.0 MB/512M  8001  https://myapp.example.com  v4 running 2h ago
blog    running  v12  77ac1f0  3d      58.8 MB/512M   8003  https://blog.example.com   v12 running 3d ago

SERVICE      IMAGE        STATE    MEM       ENDPOINT           LINKED TO  LAST BACKUP
pg:db        postgres:17  running  209.3 MB  192.168.66.1:6001  [myapp]    9h ago
```

Anything wrong shows up underneath: a crashed app, a database that's down,
low disk, memory over 80% committed, a backup that hasn't worked lately.
`berth status myapp` zooms in on one app.

```bash
berth events -f       # deploys, crashes, restarts, webhooks and backups as they happen
```

## One-off commands

```bash
berth run myapp npx prisma migrate deploy      # a fresh container with the current image and config
berth run myapp python manage.py createsuperuser
berth exec myapp                               # a shell inside the running container
berth exec myapp node -e 'console.log(process.env.PORT)'
```

`run` exits with your command's exit code, so it's fine in scripts.

## Postgres and Redis

```bash
berth pg:create db                 # Postgres 17 in its own VM
berth pg:link db myapp             # a login for myapp, DATABASE_URL set, redeployed
```

The app now has `DATABASE_URL=postgres://myapp:…@192.168.66.1:6001/db`. Each
linked app gets its own login, but they share the tables, so several apps can
use one database.

```bash
berth pg:psql db                                # psql
berth pg:psql db -c "select count(*) from users"
berth pg:backup db                              # pg_dump → ~/.berth/backups/pg/db/<time>.dump
berth pg:restore db ~/.berth/backups/pg/db/20261025-030000.dump
berth pg:expose db 15432                        # for TablePlus and friends, at 127.0.0.1:15432
berth pg:expose db off
berth pg:upgrade db --to 18                     # dump, new Postgres, restore, relink
berth pg:unlink db myapp                        # revoke the login, remove DATABASE_URL
berth pg:destroy db                             # refuses while apps are linked (--force if you mean it)
berth pg:list
```

**Every Postgres database is dumped every night** at 03:00, and the last 14
dumps sit in `~/.berth/backups/pg/` where Time Machine will find them. The
dumps are the real backup: the live files are inside a container volume, and
copying a running database's files isn't reliable anyway. `pg:upgrade` keeps
the old data volume until you delete it, just in case.

Link a second database to the same app and its variable is named after it
(`PG_ANALYTICS_URL`); pick your own name with `--as`.

**Redis** works the same way, with `REDIS_URL` and append-only persistence.
Each linked app gets its own Redis user, so unlinking one doesn't disturb the
others:

```bash
berth redis:create cache
berth redis:link cache myapp
berth redis:cli cache GET somekey
berth redis:backup cache
berth redis:restore cache ~/.berth/backups/redis/cache/<time>.rdb
```

`pg:create` and `redis:create` take `--memory` (`512M` and `256M` by default)
and `--image` (`--image postgres:16`, for example).

## SQLite

If your app wants a SQLite file rather than a database server, give it one:

```bash
berth sqlite:add myapp
```

The app now has `DATABASE_URL=sqlite:////data/sqlite/app.db`, and
`SQLITE_PATH=/data/sqlite/app.db` for libraries that want a plain path. If
`DATABASE_URL` is already taken, pick another name with `--as SQLITE_URL`.

```bash
berth sqlite:shell myapp                        # sqlite3, inside the app
berth sqlite:shell myapp "select count(*) from notes"
berth sqlite:backup myapp                       # → ~/.berth/backups/sqlite/myapp/<time>.db
berth sqlite:restore myapp ~/.berth/backups/sqlite/myapp/20261028-030000.db
berth sqlite:list
berth sqlite:remove myapp                       # the data stays unless you add --delete-data
```

The file lives on a disk image that only one container can attach at a time.
That matters because every Berth container is its own little VM, and
SQLite's locks don't reach from one VM into another. Two containers sharing
a SQLite file in an ordinary folder can both write at once and corrupt it. So
an app with SQLite runs one container at a time:

- **Deploys stop the old version before starting the new one.** Visitors
  wait a few seconds rather than getting an error. If the new version doesn't
  come up, the old one starts again.
- **Scheduled jobs and `berth run` happen inside the running app**, not in a
  new container.
- **Workers can use the database too**: they run inside the app's
  container, like every app's [workers](#workers).

**It's backed up every night** along with Postgres, and before every release
command. Each backup is one ordinary SQLite file in
`~/.berth/backups/sqlite/`, checked on the Mac before it's kept. Backups
happen while the app keeps running, as long as its image has `sqlite3` or
Python (most do). If it has neither, the nightly backup stops the app for a
few seconds instead, and `sqlite:backup` asks you to add `--stop`.

Dev versions and previews get their own empty database, and your release
command sets it up.

## Migrations

Tell Berth how your app migrates its database and it'll run that once per
release, after the build and before the new code sees any traffic. Add a
`Procfile` to the repo with a `release:` line:

```
release: npx prisma migrate deploy
```

Any tool works: `alembic upgrade head`, `python manage.py migrate --noinput`,
`bin/rails db:migrate`, `npx knex migrate:latest`, or a script of your own.
No Procfile? Set it as config instead:
`berth config:set myapp BERTH_RELEASE_COMMAND="alembic upgrade head"`.

Then on every deploy Berth:

1. backs up each Postgres database the app is linked to,
2. runs the command in a fresh container from the new image, with the new
   release's config (so `DATABASE_URL` and friends are there), and streams
   the output,
3. starts the new release only if the command worked. If it didn't, the
   deploy stops, the running version carries on serving, and the output
   tells you what broke and where the backup is.

```
-----> Backed up pg:chirpdb before the release command (0.2s, 5.1 KB)
-----> Running release command: node migrate.js
       applied 004_add_mood (3ms)
       1 migration(s) applied
-----> Release command finished in 0.8s
-----> Starting berth-chirp-5-k2x9 on 127.0.0.1:8002
```

`berth logs myapp --release` shows the latest run. If your app creates its
tables when it starts up, move that into the release command. Otherwise a
half-done migration leaves you with a crashing container and nothing to undo
it with. Config changes (`config:set`, `pg:link`) run the command again too,
so it has to be safe to repeat. Every migration tool's "apply what's pending"
command is.

If Berth spots a migration tool (Prisma, Drizzle, Knex, Alembic, Django,
Rails) but no release command, the deploy prints the line to add. It won't
run migrations you didn't ask for.

### Write migrations the old code can live with

Because deploys have no downtime, the old release is still serving for a few
seconds against the migrated database. So every migration has to work with
both the old code and the new:

- **Adding** a column or table is fine. Make new columns nullable or give
  them a default.
- **Removing** one takes two releases: stop using it, then drop it later.
- **Renaming** is add, copy, switch the code, then drop.

### Try it on the dev version first

The [dev version](#dev-versions) runs the release command against its own
database, so you can try a migration before committing it:

```bash
berth dev:db:copy myapp     # copy production's data into the dev database, then migrate it
berth dev:push              # build your working tree, run the release command, start it
```

`dev:push` exits with code 3 if the release command fails, with the error in
the output (and in `--json`). This is the loop to give an agent: write the
migration, `dev:push`, fix until it exits 0, commit.

### Rolling back data

Since there's a backup from just before each release command, you can undo a
migration along with the code:

```bash
berth rollback myapp --with-data
```

This swaps back to the previous release, then replaces the database with the
backup taken just before the newer release migrated it. Anything written
since then is gone, so Berth asks first, and saves the current data to
`~/.berth/backups/pg/<db>/` before restoring. Without `--with-data`, a
rollback leaves the data alone, which is what you want whenever the migration
was backward compatible.

The last 14 pre-release backups per app are kept in
`~/.berth/backups/pg/<db>/releases/<app>/`. If a database is too big to dump
on every deploy, `berth config:set myapp BERTH_RELEASE_BACKUP=off` skips it
(and `--with-data` then has nothing to restore).

## Workers and scheduled jobs

[berth_hello_python](https://github.com/dps/berth_hello_python) has a worker
and two cron jobs if you want to see both working.

### Workers

Background processes go in the `Procfile`, next to the web process:

```
web: node server.js
worker: node worker.js
clock: node clock.js
```

Anything other than `web` and `release` is a worker. Each one runs as
another process inside the app's container, with the same build, config and
storage, and gets restarted (with backoff) if it exits. Workers move to each
new release as it goes live, follow rollbacks, and stop and start with the
app. The old release's workers stop before the new ones start, so a job never
gets picked up twice. Each knows its name from `BERTH_PROCESS` (`worker.2`).
You
still need a web process, and it's always the image's own command: Berth
doesn't read the `web:` line, so it's fine to keep it for other tools.

```bash
berth ps:scale myapp worker=3      # three of them
berth ps:scale myapp clock=0       # none
berth status myapp                 # each worker and how long it's been up
berth logs myapp -f                # worker lines say which: v4 worker.2: …
```

Workers share the app's memory and CPUs, so if they're heavy, give the app
more with `BERTH_MEMORY` and `BERTH_CPUS`. The image needs `/bin/sh`, `tr`
and `grep`, which every common base image has.

### Scheduled jobs

Put a `cron` list in `app.json` at the root of the repo, in
[Dokku's format](https://dokku.com/docs/processes/scheduled-cron-tasks/):

```json
{
  "cron": [
    { "command": "node digest.js", "schedule": "0 7 * * *" },
    { "command": "python cleanup.py", "schedule": "@hourly" }
  ]
}
```

Schedules are ordinary five-field cron (minute hour day month weekday, with
ranges, steps, lists and names like `mon-fri`) or `@hourly`, `@daily`,
`@weekly` and `@monthly`, in the Mac's local time. They ship with the code, so
a rollback brings back the schedule that went with it, and a typo fails the
deploy rather than quietly never running.

Each run is a fresh container from the current release, with the app's
config and storage. If a job is still running when its next turn comes round,
that turn is skipped. When a job starts failing you get one notification,
and another when it works again, not one per run (a job that runs every
minute would otherwise send sixty an hour). `BERTH_CRON_TIMEOUT` (an hour by
default) stops runaways. Runs missed while the Mac was off don't get made up.

```bash
berth cron myapp                   # the jobs, when they next run and how the last run went
berth cron:run myapp 1             # run job 1 now and wait for it
berth logs myapp --cron            # every run's output
```

Dev versions and previews don't run schedules. A job running against a copy
of production's data could email real people. Try one by hand with
`berth cron:run myapp-dev 1`.

## Persistent storage

Containers start clean on every deploy. For uploads, generated files or
caches, mount a folder that sticks around:

```bash
berth storage:mount myapp /uploads
berth storage:list myapp
berth storage:unmount myapp /uploads    # stops mounting it; the files stay
```

The folder is `~/.berth/data/apps/myapp/uploads/` on the Mac: a plain folder
that Time Machine and `berth backup` both pick up.

Don't keep a SQLite database in one of these; use
[`berth sqlite:add`](#sqlite). A database in a shared folder can be corrupted
when two containers write at once, which happens on every deploy.

## Domains, LAN URLs and passwords

Every app gets two addresses:

| URL | Works from |
| --- | --- |
| `https://myapp.example.com` | anywhere, through the Cloudflare tunnel |
| `https://myapp.your-mac.local` | your LAN, advertised over Bonjour, with Caddy's local certificate |

`your-mac` is the Mac's local hostname (System Settings → General → Sharing).
To get a padlock on LAN URLs on the Mac itself, run `berth trust-ca`. On your
other devices, install `~/.berth/caddy/pki/authorities/local/root.crt` as a
trusted certificate.

### Custom domains

```bash
berth domains:add myapp www.mysite.com
berth domains:list myapp
berth domains:remove myapp www.mysite.com
```

Anything under your main domain just works. For a domain in another zone on
your Cloudflare account, log in to that zone once (it opens the browser) and
then add it:

```bash
berth cf:login mysite.com
berth domains:add myapp www.mysite.com
```

Berth creates the DNS record and updates the tunnel live. It won't replace a
record that's already there (say `www` points at your old host) unless you
add `--force`. If the domain's DNS lives somewhere else, `domains:add` prints
the record to create (`www.mysite.com CNAME <tunnel-id>.cfargotunnel.com`),
but the zone still has to be proxied through Cloudflare for the tunnel to
serve it.

### LAN-only apps and passwords

```bash
berth create admin-tool --repo you/admin-tool --private   # LAN only, no public URL
berth auth:set myapp alice:correct-horse-battery          # HTTP basic auth, enforced by Caddy
berth auth:unset myapp
```

## Sign in with Google

I got tired of writing the same OAuth code for every little app, so Berth
does it once. It puts Google sign-in in front of any app and tells the app
who's signed in with request headers.

### Set it up once

```bash
berth auth:setup google
```

Google doesn't let programs create OAuth clients, so this walks you through
about a minute in the Google Cloud console: create a "Web application" client
with the redirect URI it prints (`https://auth.<your domain>/callback`),
publish the consent screen, and paste in the client ID and secret. Every app
shares that one client. Then open `https://auth.<your domain>` and sign in to
check it works.

If `auth.<your domain>` is already taken, pick another name:
`berth auth:setup google --host signin.<your domain>`. Berth won't take over a
name that already has its own DNS record.

### Choose who gets in

```bash
berth access myapp public                      # anyone; the app still learns who's signed in
berth access myapp signed-in                   # any Google account
berth access myapp allow you@gmail.com @family @example.com
berth access myapp /admin/* allow you@gmail.com   # a rule for some paths (* is a prefix)
berth access myapp /admin/* --remove
berth access myapp                             # show the policy
berth access myapp off                         # no sign-in at all
```

`allow` takes email addresses, Google Workspace domains (`@example.com`) and
groups. It won't take `@gmail.com`: Google doesn't vouch for a domain on
personal accounts, so it would match nobody. Use `signed-in` for "any Google
account". Make a group once and use it anywhere:

```bash
berth auth:group family alice@gmail.com bob@gmail.com
```

Changes to allow lists and groups apply on the visitor's next request. Anyone
not signed in gets sent to Google first (API calls get a 401 instead), and
anyone signed in without access sees a "No access" page with a button to
switch accounts.

### In your app

Requests arrive with these headers. Berth strips any `X-Berth-*` headers a
visitor sends, so you can trust them.

| Header | Value |
| --- | --- |
| `X-Berth-User-Id` | `google:<id>`, stable for the life of the account: key your users table on this |
| `X-Berth-Email` | `you@gmail.com` |
| `X-Berth-Name` | display name, URL-encoded (`decodeURIComponent` it) |
| `X-Berth-Picture` | avatar URL |
| `X-Berth-Identity` | all of the above as a 5-minute signed JWT (EdDSA), if you'd like to verify; the key is at `/_berth/jwks.json` |

On a `public` app the headers are missing for anonymous visitors. That's the
pattern for a site anyone can read but only signed-in people can post to:
check for the header on your write endpoints, and return 401 if it's not
there.

Apps with a policy also get these paths. On an app with no policy they answer
404, with a note saying `berth access myapp public` turns them on (and keeps
the app open to everyone):

| Path | What |
| --- | --- |
| `/_berth/login?return=/somewhere` | sign in, then come back |
| `/_berth/logout?return=/path` | sign out of every app (POST it from a form; a GET shows a button), then go back to the path, or to a "Signed out" page if that path needs sign-in |
| `/_berth/me` | who's signed in, as JSON, for single-page apps |

Once you've signed in to one app, the next is a quick redirect with no Google
prompt. Sessions last 30 days and renew as long as they're used.
`berth auth:sessions` lists them, `berth auth:revoke <email>` signs someone out
everywhere, and `berth auth:status` shows the setup, groups and every app's
policy. [Chirp](https://github.com/dps/berth_chirp_node) is a complete example.

## Dev versions

This is the part I use most. Every app has a dev version at
`https://<app>-dev.<your domain>`, a place to try changes before they're
committed. It's built so coding agents can test their own work. From the
app's repo:

```bash
berth dev:push
```

That deploys the working tree exactly as it is on disk, uncommitted edits and
new files included (`.gitignore` is respected, so `.env` and `node_modules`
stay out). Your branch, index and stash aren't touched. A small change is
live in about 5 seconds. Production is never affected; it still only deploys
from pushes to its branch.

For agents, `dev:push` waits until it's done and has useful exit codes:

| Exit code | Meaning |
| --- | --- |
| 0 | live |
| 1 | the build failed (output above) |
| 2 | it built but didn't start (its last log lines are above) |
| 3 | the [release command](#migrations) failed, e.g. a migration |

`--json` prints the result (`status`, `stage`, `url`, `release`, the base
commit and a summary of what's uncommitted) on stdout, with progress on
stderr. A failed push leaves the previous dev release serving.

`berth dev:agents myapp` prints a block for the repo's `AGENTS.md` that tells
an agent how to deploy, read logs and get in. Apps made with
[`berth new`](#starting-a-new-app) already have it.

### Who can reach it

Dev versions run unreviewed code, so they're not public. With
[sign-in](#sign-in-with-google) set up they're open to the `@owners` group
(`berth auth:group owners you@gmail.com`); without it, they're LAN only.
Agents can't click through Google, so give them a token:

```bash
berth dev:token myapp --as test@example.com     # prints export BERTH_DEV_TOKEN=…
curl -H "Authorization: Bearer $BERTH_DEV_TOKEN" https://myapp-dev.example.com/api/me
berth dev:token myapp --revoke                  # revoke them all
```

With `--as`, requests are signed in as that pretend user
(`X-Berth-User-Id: dev:test@example.com`), so an agent can test the signed-in
parts of the app too. Tokens only work on dev versions and previews.

### Its data is its own

A dev version never touches production's data:

- **Postgres:** its own database (`<db>_dev`) in the same Postgres, with a
  login that can't connect to production's. It starts empty.
  `berth dev:db:copy myapp` replaces it with a copy of production's and runs
  the [release command](#migrations) on it.
- **Redis:** its own small Redis.
- **Storage:** its own folders.
- **Config:** copied from the app when the dev version is first made, minus
  the database URLs. API keys come along. Change it with
  `berth config:set myapp-dev KEY=value`.

### It's just another app

The dev version is the app `myapp-dev`, so every command works on it:
`berth logs myapp-dev -f`, `berth run myapp-dev npm run migrate`,
`berth restart myapp-dev`. It keeps 3 releases, and it doesn't run
[scheduled jobs](#scheduled-jobs).

To save memory it goes to sleep after 12 hours with no pushes and no
requests, and the next `dev:push` wakes it up. Change that with
`dev_idle_hours` in `~/.berth/config.json`.

`berth dev:reset myapp` deletes the dev version and its data, so the next
push starts fresh from the app's current config. `berth dev:off myapp`
removes the dev address altogether.

On your laptop, `berth dev:push` in the repo sends the snapshot to the Mac
over SSH and deploys it there, with the same exit codes and `--json`. See
[Working from your laptop](#working-from-your-laptop).

## Pull request previews

```bash
berth previews:on myapp
```

Now each open pull request into the app's branch gets its own app,
`myapp-pr-<number>`, at `https://myapp-pr-12.<your domain>`. It's built from
the PR's latest commit, redeployed on every push to it, and destroyed when
the PR is merged or closed. A check on the PR links to it, and Berth leaves a
comment the first time.

Previews work like [dev versions](#dev-versions). They start with the app's
config, get their own empty databases (which the release command sets up),
run workers but not scheduled jobs, sleep when idle, and sit behind the same
access rule (`@owners` with sign-in, LAN only without), so dev tokens work on
them too.

- PRs from **forks** never get a preview. That would be running a stranger's
  code on your Mac.
- At most 3 run at once (`--max 5` to change it). A PR over the limit gets a
  check saying so.
- They follow GitHub's pull request webhook, and check every 5 minutes for
  anything missed. They need a GitHub repo and Berth's GitHub login.

```bash
berth previews myapp       # the previews and how they're doing
berth previews:off myapp   # stop, and destroy the ones that exist
```

## AI for your apps

Berth can give any app an LLM: small models running on the Mac itself, cloud
models with keys you add once, or both. Apps reach all of them through one
gateway in Berth using the OpenAI or Anthropic SDK they'd use anyway, and
they never see your provider keys.

### Set it up once

```bash
berth ai:setup
```

This installs [Ollama](https://ollama.com) with Homebrew and starts it with
the Mac (it asks first), then downloads a model that fits: `qwen3:8b` with
16 GB of memory or more, `qwen3:4b` below that. Pick another with
`--model gemma3:4b`, or skip local models with `--cloud-only`. Local models
run on the Mac's GPU, outside the app containers, which can't use it.

Add whichever cloud providers you want (the key prompt is hidden):

```bash
berth ai:key anthropic          # also openai, gemini, openrouter, groq, mistral
berth ai:key together --url https://api.together.xyz/v1    # anything OpenAI-compatible
```

### Give an app access

```bash
berth ai:enable myapp
```

The app gets its own token and the usual variables, and redeploys:

| Variable | Value |
| --- | --- |
| `OPENAI_BASE_URL` | `http://<gateway>:7790/v1` |
| `OPENAI_API_KEY` | `berth_ai_…` (this app's token) |
| `ANTHROPIC_BASE_URL` | `http://<gateway>:7790/anthropic` |
| `ANTHROPIC_API_KEY` | the same token |

The official SDKs read those by themselves, so there's nothing Berth-specific
in your code:

```js
import OpenAI from "openai";
const client = new OpenAI();   // reads OPENAI_BASE_URL and OPENAI_API_KEY
const reply = await client.chat.completions.create({
  model: "default",
  messages: [{ role: "user", content: "Hello!" }],
});
```

Python's `openai` and `anthropic` packages work the same way. Every provider
speaks the OpenAI format (`/v1/chat/completions`, `/v1/embeddings`,
`/v1/models`), and Claude models also speak Anthropic's own API, for tools,
prompt caching and extended thinking.

### Choosing models

Apps ask for a model by name, like `local/qwen3:8b` or
`anthropic/claude-sonnet-5-5`, or by an **alias** you set up once for every
app:

```bash
berth ai:alias smart anthropic/claude-sonnet-5-5
berth ai:alias fast  groq/llama-3.3-70b-versatile
berth ai:models                  # aliases, local models, providers
berth ai:pull nomic-embed-text   # another local model, e.g. for embeddings
```

`ai:setup` points `default` at the local model. I'd write apps against
aliases (`"default"`, `"smart"`) rather than model names: change what an
alias points at and every app picks it up immediately, no redeploy.

### Limits and usage

```bash
berth ai:enable myapp --models default,smart --monthly-tokens 2000000 --rpm 60
berth ai:usage                   # requests and tokens per app and model this month
berth ai:status                  # what's running, what's loaded, each app's usage
berth ai:disable myapp           # revoke its token
```

`--models` takes aliases, `provider/model` or `provider/*`. An app over a
limit gets a 429 that says which limit and how to raise it. Usage records
tokens and timings, never prompts or replies.

[Dev versions](#dev-versions) of an AI app get their own token that only
reaches local models, so an agent testing changes can't run up a cloud bill
(`berth ai:enable myapp-dev --models 'local/*,smart'` lifts that).

### Memory

A loaded local model takes real memory: about 5 GB for an 8B model, 2.5 GB
for a 4B one. Ollama lets go of it after 5 idle minutes. `berth status` shows
what's loaded, counts it, and warns when apps, databases and the model
together get close to the Mac's RAM. On an 8 GB Mac, use a cloud provider (or
a really small model).

The gateway listens on port 7790 on the containers' network, and only
containers on this Mac can reach it. To change the port, set `ai_port` in
`~/.berth/config.json` and run `berth reload`. To use Ollama on another
machine, set `ollama_url` in `~/.berth/ai/config.json`.

## The admin page

```bash
berth admin:enable --owner you@gmail.com
berth admin                      # opens https://admin.<your domain>
```

A live web page showing everything on your Berth, for the people in the
`@owners` [sign-in](#sign-in-with-google) group:

- **Overview:** whether everything's up (containers, proxy, tunnel, disk,
  memory, AI), warnings, traffic and latency across all apps for the last day,
  every app at a glance, your databases, and recent deploys and crashes.
- **System load:** the Mac's load average, CPU, memory in use and swap,
  charted over the last hour or day, and how much CPU each app and database
  is using. It samples every 15 seconds and keeps a day.
- **Each app:** requests, visitors, errors and latency for the last hour, day
  or week compared with the one before, with charts; top pages, referrers,
  countries and browsers; requests as they arrive; releases, output and setup.
- **Events** and **AI** (models, aliases, each app's token use).

The numbers come from the request logs Berth already writes. Nothing gets
added to your apps' pages (no script, no cookie), and nothing about your
visitors leaves the Mac. A "visitor" is a distinct IP address and browser;
crawlers, scripts and uptime checkers are counted separately. Countries need
the Cloudflare tunnel. It keeps 7 days.

It's read-only on purpose: deploys, rollbacks and config changes stay in the
terminal, and config values never appear, only their names. Serve it
somewhere else with `--host dash.example.com`, or turn it off with
`berth admin:disable`.

## Living in a closet

Berth is built to run for months without anyone looking at the screen.

- **Crashed apps restart.** An app that exits is restarted with backoff (1
  second, growing to 60). After 10 crashes without 10 healthy minutes in
  between, Berth gives up: the app is marked `crashed`, its URL answers 503,
  you get a notification, and it stays down until you deploy or
  `berth restart` it. Databases are restarted whenever they're down.
- **Reboots come back.** When `berthd` starts, it starts Apple's container
  system, then the databases, then every app, oldest first.
- **Network glitches fix themselves.** Apple's container network occasionally
  loses its route to the internet (builds fail, apps can't reach outside
  APIs) while the Mac itself is fine. `berthd` checks every 5 minutes, and
  after two failures in a row restarts the container system. Apps are down
  for about 6 seconds and you get a notification. Set
  `"auto_heal_network": false` in `~/.berth/config.json` if you'd rather just
  be told.
- **Full disks don't happen.** Berth refuses to deploy with less than 2 GB
  free rather than filling the boot volume.

### Notifications

```bash
berth notify:set https://hooks.slack.com/services/…    # Slack, Discord or ntfy.sh
berth notify:set off
```

You hear about deploys that succeed or fail, apps or workers stuck
crash-looping, scheduled jobs that fail, and nightly or off-Mac backups that
fail. `notify:set` sends a test message first.

### Reboots and FileVault

This is the one real trade-off. With FileVault on, macOS turns off automatic
login, and after a power cut the Mac sits at a password screen before
anything (Berth included) can start. So pick one:

- **FileVault on (the default, and what I'd do).** Crashes and restarts
  recover on their own. For planned reboots, like macOS updates, use
  `berth reboot`: it unlocks the disk for exactly one boot
  (`fdesetup authrestart`), so the Mac comes back with nobody there. After an
  *unplanned* power cut someone has to type the password. A small UPS makes
  that rare.
- **Unattended (`berth init --unattended`).** FileVault off, automatic login
  on (`init` shows you where). Power cuts recover with nobody home, but the
  disk, secrets and all, isn't encrypted.

`berth doctor` tells you which mode you're in and whether it's set up right.

### Keeping it tidy

```bash
berth doctor             # checks everything Berth relies on and prints a fix for each problem
berth doctor --images    # and which base images have updates
berth gc                 # clear out old release images, expired dumps and leftover build files
```

## Working from your laptop

The Mac can live in the closet while you, your editor and your agent work on
a laptop. Install the CLI there too, but don't run `init`: use the Homebrew
formula on a Mac that meets [the requirements](#what-you-need), or
`go install github.com/dps/berth/cmd/berth@latest` anywhere else Go runs
(Intel Macs and Linux included). Then tell it where the Mac is:

```bash
export BERTH_HOST=you@your-mac.local     # whatever name ssh knows the Mac by
berth status
```

Every `berth` command then runs on the Mac over SSH, attached to your
terminal, so prompts, `berth logs -f` and `berth run` shells all work. You
need Remote Login on the Mac (System Settings → General → Sharing) and SSH
keys, so there's no password prompt.

Inside an app's repo, the `berth` git remote does the same job as
`BERTH_HOST`, so an agent working there doesn't need anything set. `berth new`
adds it; for an existing clone, run `berth git:remote myapp` once in the repo.

A few commands do their work on the laptop instead: `berth new` and
`dev:push` (they need the files here), `upgrade`, `git:remote` (it adds the
remote here), `admin` (it opens your browser here), and `config:import`,
which sends the laptop's `.env`. Any other path you pass, as in
`berth pg:restore db dump.sql`, is a path on the Mac.

## Backups and moving to a new Mac

Database backups happen every night on their own ([Postgres](#postgres-and-redis), [SQLite](#sqlite)).
For everything else there's one archive:

```bash
berth backup                         # → ~/berth-backup-<time>.tar.gz
berth backup /Volumes/USB/berth.tar.gz
```

It holds everything needed to rebuild: app settings and config, release
history, fresh database backups and your storage folders. It leaves out
anything that can be fetched or rebuilt again (git mirrors, images, logs). It
has your secrets in it, so keep it somewhere safe.

To restore it, on the same Mac or a new one:

```bash
berth init --domain example.com      # on a Mac with no apps yet
berth restore berth-backup.tar.gz
```

Everything comes back: each app's settings, config, domains, access rules,
workers and AI access; sign-in groups and sessions; Postgres, Redis and
SQLite from their backups; storage folders; and the GitHub, Cloudflare and Google logins.
Every app is redeployed from its repo at the commit that was live. A few
things come back lazily: apps without a repo come back with their settings
(push to them to deploy), dev versions on their next `dev:push`, previews
with their pull requests, and local AI models with `berth ai:setup`. LAN
addresses change to the new Mac's name.

### Off-Mac backups

That archive lives on the Mac, which doesn't help much if the Mac dies or
walks off. So send one somewhere else every night:

```bash
berth offsite:setup --r2 <cloudflare account id> --bucket my-berth-backups
```

After the 03:00 dumps, Berth makes the same archive as `berth backup`,
encrypts it on the Mac, uploads it, checks it arrived intact, and keeps the
newest 14 (`--keep` changes that). Any S3-compatible storage works. For
Cloudflare R2, create a bucket and an R2 API token with *Object Read & Write*
on it. For anything else (Backblaze B2, AWS S3, MinIO), pass `--endpoint`.
Setup asks for the secret key without echoing it, and tries writing, reading,
listing and deleting a test file before it saves anything.

Setup then prints a **recovery code** (`berthrestore_…`). It's the bucket's
address and credentials plus the key that encrypts the backups, all in one
string. **Save it somewhere that isn't the Mac.** A password manager is
perfect. The storage provider only ever sees ciphertext, and without the code
nobody can read the backups, including you. `berth offsite:key` prints it
again; it changes if you rerun setup with new credentials.

```bash
berth offsite:status                 # where they go, the last upload, what's in the bucket
berth offsite:run                    # upload one now
berth offsite:get                    # download and decrypt the latest, ready for berth restore
berth offsite:off                    # stop (the bucket keeps its backups)
```

`berth status` warns if no upload has worked for two days, and a failed
upload sends a notification.

### Moving to a new Mac

With off-Mac backups, moving is one command on the new Mac, once Homebrew and
Berth are installed:

```bash
berth init --restore                 # asks for the recovery code
```

It fetches the newest backup and puts the old Mac's settings and logins in
place. That means no `--domain`, and no Cloudflare or GitHub logins in the
browser, since it reuses the same tunnel and token. Then it finishes setting
up and restores every database and app as above.

Turn Berth off on the old Mac first (`berth uninstall`). Otherwise both Macs
share the tunnel and split your traffic between them, which is confusing.

### Uninstalling

```bash
berth uninstall              # stop everything and remove the background services; keeps ~/.berth
berth uninstall --purge      # also delete every app, webhook, database volume, image and ~/.berth
```

## Upgrading

```bash
berth upgrade            # upgrade the way you installed it
berth upgrade --check    # just compare with the latest release
```

With Homebrew that's `brew update` then `brew upgrade berth`, and Berth checks
the new version really got installed. The running `berthd` notices its new
binary, waits for any deploy in progress, and restarts itself. Your apps keep
running the whole time.

From a laptop, `berth upgrade` upgrades the laptop and then offers to do the
Mac. If the two are on different versions, commands tell you.

Upgrading from 0.1.0 or 0.1.1? Run `brew update && brew upgrade berth` once by
hand. Those versions' `berth upgrade` forgot the `brew update`.

## Commands

Add `--json` to any non-interactive command for machine-readable output.
Progress goes to stderr and the result to stdout, which is what scripts and
agents want. `berth <command> --help` has more on each.

| Command | What it does |
| --- | --- |
| **Setup** | |
| `berth init --domain d` | one-time setup; safe to rerun ([flags](#setting-up)) |
| `berth init --restore` | set up a new Mac from your off-Mac backups |
| `berth doctor [--images]` | check everything and print fixes |
| `berth cf:login <zone>` | connect another Cloudflare zone, for custom domains |
| `berth trust-ca` | trust Caddy's local CA in your login keychain |
| `berth upgrade [--check]` | upgrade Berth here (and, from a laptop, on the Mac) |
| `berth uninstall [--purge]` | remove Berth |
| **Apps** | |
| `berth new <app> [--stack node\|python\|go\|static] [--repo r] [--public] [--no-github] [--no-deploy] [-y]` | start an app: starter code, `AGENTS.md`, a GitHub repo, the first deploy |
| `berth create <app> [--repo r] [--branch b] [--path p] [--private] [--no-webhook] [--no-deploy]` | register and deploy an app |
| `berth deploy <app> [--ref r] [--no-cache]` | build and release now |
| `berth rollback <app> [n] [--with-data]` | back to the previous (or nth) release; `--with-data` restores its databases too |
| `berth restart \| stop \| start <app>` | lifecycle |
| `berth releases <app>` | release history |
| `berth destroy <app> [--keep-data]` | delete an app, its webhook and images |
| `berth dockerfile <app>` | print the Dockerfile Berth would use |
| `berth git:remote <app> [--host h]` | add a `berth` remote, for `git push berth`, `dev:push` and running commands from a laptop |
| `berth app:set-key <app>` | use an SSH deploy key for this repo |
| `berth app:set-repo <app> <repo> [--branch b]` | deploy from another repo or branch |
| `berth ps:scale <app> <type>=<n>…` | how many of each Procfile worker to run |
| `berth cron <app>` · `cron:run <app> <n>` | scheduled jobs from `app.json`; run one now |
| **Config** | |
| `berth config:set <app> K=v … [--no-restart] [--build-arg]` | set variables and redeploy |
| `berth config:unset <app> K …` | remove variables |
| `berth config:get <app> K` | print one variable |
| `berth config:show <app> [--reveal] [--shell]` | list variables |
| `berth config:import <app> <file\|-> [--replace]` | load a `.env` file (`-` reads stdin) |
| **Logs and status** | |
| `berth logs <app> [-f] [-n N] [--since d] [--build [n]] [--release [n]] [--access] [--cron]` | app, build, release-command, access or scheduled-job logs |
| `berth logs --all [-f]` | every app's logs |
| `berth status [app]` | overview, or one app |
| `berth events [-f]` | event log |
| **Running things** | |
| `berth run <app> <cmd…>` | one-off command in a fresh container (inside the running one, for apps with SQLite) |
| `berth exec <app> [cmd…]` | command or shell in the running container |
| **Data** | |
| `berth pg:create \| link \| unlink \| psql \| backup \| restore \| expose \| upgrade \| destroy \| list` | Postgres |
| `berth redis:create \| link \| unlink \| cli \| backup \| restore \| expose \| destroy \| list` | Redis |
| `berth sqlite:add \| remove \| shell \| backup \| restore <app>` · `sqlite:list` | an app's own SQLite database |
| `berth storage:mount \| unmount \| list <app> [path]` | persistent folders |
| **Hostnames and access** | |
| `berth domains:add \| remove \| list <app> [host] [--force]` | custom domains |
| `berth auth:set <app> user:pass` · `auth:unset <app>` | basic auth |
| `berth access <app> [path] [public \| signed-in \| allow who... \| off] [--remove]` | Google sign-in policy |
| `berth auth:setup google [--host h]` · `auth:status` · `auth:group <name> [who...]` · `auth:sessions` · `auth:revoke <email>` | sign-in setup, groups, sessions |
| **Dev versions and previews** | |
| `berth dev:push [app] [--json]` | deploy the working tree to `<app>-dev` |
| `berth dev:token <app> [--as email] [--revoke]` · `dev:db:copy` · `dev:reset` · `dev:agents` · `dev:off` · `dev:on` | manage dev versions |
| `berth previews <app>` · `previews:on <app> [--max n]` · `previews:off <app>` | pull request previews |
| **AI** | |
| `berth ai:setup [--model m] [--cloud-only]` | install Ollama, download a local model, start the gateway |
| `berth ai:key <provider> [key] [--url u] [--remove]` | add a cloud provider's key (or any OpenAI-compatible URL) |
| `berth ai:alias [name] [provider/model] [--remove]` | name models for apps; changes apply at once |
| `berth ai:enable <app> [--models …] [--monthly-tokens n] [--rpm n]` · `ai:disable <app>` | give or revoke an app's access |
| `berth ai:status` · `ai:models` · `ai:pull <model>` · `ai:usage [app] [--month m]` | what's available, downloads, usage |
| **Running the Mac** | |
| `berth admin` · `admin:enable [--host h] [--owner email]` · `admin:disable` | the admin page for `@owners` |
| `berth backup [file]` · `berth restore <file>` | archive and restore everything |
| `berth offsite:setup` · `offsite:status` · `offsite:run` · `offsite:get [backup]` · `offsite:key` · `offsite:off` | encrypted nightly backups to S3-compatible storage |
| `berth notify:set <url\|off>` | notifications |
| `berth gc` | clean up old images, dumps and build files |
| `berth reboot` | planned reboot that comes back with nobody there |
| `berth reload` | re-read `~/.berth` after you've edited files by hand |
| `berth completion zsh \| bash \| fish` | shell completions (Homebrew installs them) |

## Troubleshooting

**Start with `berth doctor`.** It checks each moving part and prints the
command that fixes whatever's wrong.

**A deploy failed.** The error ends with the app's last log lines; for the
whole story, `berth logs myapp --build` then `berth logs myapp`. The previous
release is still serving. The usual suspects: the app listens on `localhost`
instead of `0.0.0.0`, or ignores `$PORT`; it's slow to boot (raise
`BERTH_HEALTH_TIMEOUT`); or `/` returns a 5xx (point `BERTH_HEALTHCHECK` at a
health endpoint).

**A push didn't deploy.** Run `berth events`. You'll see `webhook` entries if
GitHub reached the Mac, or `webhook.rejected` for a bad signature. Without
webhooks, polling picks the push up within a minute. A monorepo app ignores
pushes that don't touch its path.

**`berth: berthd is not running`.** Start it with
`launchctl kickstart -k gui/$UID/dev.berth.berthd`. Its log is
`~/.berth/logs/berthd.log`.

**The public URL doesn't work but the `.local` one does.** That's the tunnel.
`berth doctor` checks it's connected and that the wildcard DNS record
resolves. cloudflared's log is `~/.berth/logs/cloudflared.log`.

**An app says `crashed`.** It failed to start 10 times in a row. Look at
`berth logs myapp`, fix it, then `berth restart myapp` or push.

**`berth upgrade` says it's done but the version didn't change.** You're on
0.1.0 or 0.1.1: run `brew update && brew upgrade berth` once.

**I edited files in `~/.berth` by hand.** Run `berth reload` and `berthd`
rebuilds the proxy and tunnel config and the set of running containers from
the files.

### Environment variables

| Variable | Use |
| --- | --- |
| `BERTH_HOST` | on a laptop, the Mac to run commands on, as `ssh` knows it (`you@your-mac.local`) |
| `BERTH_HOME` | use a different state folder (default `~/.berth`) |
| `BERTH_DEBUG=1` | `berthd` logs every external command and how long it took |
| `BERTH_GITHUB_CLIENT_ID` | GitHub OAuth app for the browser login in `init` |
| `BERTH_CONTAINER_BIN` | path to the `container` binary |
| `BERTH_NO_MDNS=1` | don't advertise `.local` hostnames |

## Example apps

Each one is a small repo with no Dockerfile, so Berth's stack detection does
the work. Each README lists the `berth` commands to deploy it.

| Repo | Stack | Shows |
| --- | --- | --- |
| [berth_hello_python](https://github.com/dps/berth_hello_python) | FastAPI | config, Postgres, Redis, storage, a worker, cron jobs, crash recovery |
| [berth_chirp_node](https://github.com/dps/berth_chirp_node) | Express + React (Vite) | a little Twitter clone with Google sign-in, migrations and an AI summary of the timeline |
| [berth_hello_go](https://github.com/dps/berth_hello_go) | Go standard library | a static binary on Alpine |
| [berth_hello_rust](https://github.com/dps/berth_hello_rust) | Rust + axum | cached dependency builds (4 second redeploys) |

## How it works

```
 GitHub ──webhook──▶ Cloudflare ──tunnel──▶ cloudflared ─┬─▶ berthd 127.0.0.1:7777   (hooks.<domain>)
 visitor ───────────▶   edge                             └─▶ Caddy  127.0.0.1:7780   (*.<domain>)
 LAN visitor ─────────────────────────────────────────────▶ Caddy  :443, :80 → 443   (*.<mac>.local)
                                                                 │
 berth CLI ──unix socket──▶ berthd ──container build/run──▶ app VMs on 127.0.0.1:8001–8999
                                   └──Caddy admin API             │ berth network
                                                                  ▼
                                               pg-<name>, redis-<name> VMs on the network gateway
```

- **`berthd`** is the brain. It takes webhooks, builds with `container build`,
  runs each release in its own VM, health-checks it, and switches traffic by
  updating a DNS SRV record that Caddy re-reads every second (so a deploy
  never reloads Caddy). Then it watches everything. It's the only thing that
  writes to `~/.berth`.
- **Caddy** routes each hostname to the right app, does HTTPS on the LAN,
  compresses responses, writes access logs and enforces basic auth.
- **cloudflared** keeps an outbound connection to Cloudflare, so the Mac
  needs no open ports. HTTPS for public URLs ends at Cloudflare's edge.
- **Every app and database is its own lightweight VM** with its own Linux
  kernel, so a compromised app can't get at the others or at macOS.

All the state is plain files you can read:

```
~/.berth/
  config.json              domain, ports, defaults
  apps/<app>/app.json      repo, branch, mounts, links, domains
  apps/<app>/env           live config (mode 0600)
  apps/<app>/releases/     one JSON file per release
  apps/<app>/logs/         app.log, build-<n>.log, release-<n>.log, cron.log, access.<created>.log
  apps/<app>/cron.json     each scheduled job's last run
  services/<kind>/<name>/  database settings
  backups/                 database dumps and SQLite backups
  data/apps/<app>/         storage:mount folders
  secrets/                 tokens and tunnel credentials (mode 0700)
  logs/                    berthd, Caddy and cloudflared logs
  offsite.json             the last off-Mac backup (its settings are in secrets/)
  events.log               the event log
```

The full design, including what changed along the way, is in
[design.md](design.md).

## Hacking on Berth

```bash
make build      # bin/berth and bin/berthd
make test       # go test -race ./... (runs on Linux too, with a fake container runtime)
```

To try Berth without touching `~/.berth` or the default ports:

```bash
export BERTH_HOME=/tmp/berth-dev
bin/berth init --no-tunnel --no-launchd --yes --skip-github
# edit caddy_http, caddy_https, caddy_lan_http, caddy_admin and webhook_addr
# in $BERTH_HOME/config.json, then regenerate Caddy's starting config:
rm $BERTH_HOME/caddy.json && bin/berth init --no-tunnel --no-launchd --yes --skip-github
# and in two terminals:
caddy run --config $BERTH_HOME/caddy.json
bin/berthd --debug
```

The admin page is plain files in `internal/admin/ui`. To look at it against
your real Berth without signing in, run a loopback-only preview that signs
requests as a test viewer, then open http://127.0.0.1:7799:

```bash
BERTH_ADMIN_PREVIEW=7799 BERTH_ADMIN_PREVIEW_SECS=600 go test ./internal/daemon -run TestAdminPreview
```

The S3 client has a test against a real S3-compatible server. It's skipped
unless `BERTH_S3_TEST=<endpoint>,<bucket>,<access key>,<secret>` is set;
`internal/offsite/live_test.go` shows how to run one locally with rclone.

To cut a release, `scripts/release.sh v0.2.0` runs the tests, tags, pushes,
and points the Homebrew formula at the new tarball.

## License

MIT
