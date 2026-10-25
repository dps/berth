# Berth

**A Dokku for Mac minis.** Push to GitHub and your app is live at
`https://myapp.yourdomain.com` a few seconds later, running in its own
micro-VM on a Mac in your closet. Postgres is one command away.

```bash
brew tap dps/berth https://github.com/dps/berth
brew install --HEAD berth

berth init --domain example.com           # once: tunnel, proxy, background services
berth create myapp --repo you/myapp       # once per app: webhook, hostname, first deploy
git push                                  # every deploy after that
```

That's the whole loop. No ports opened on your router, no certificates to
manage, no YAML, no Docker Desktop, and no `sudo` after setup.

```bash
berth pg:create db && berth pg:link db myapp    # a database, wired in as DATABASE_URL
berth logs myapp -f                             # follow the logs
berth rollback myapp                            # undo the last deploy
```

---

## Contents

- [What you need](#what-you-need)
- [Setting up](#setting-up)
- [Deploying an app](#deploying-an-app)
- [Releases, rollbacks and restarts](#releases-rollbacks-and-restarts)
- [Configuration](#configuration)
- [Logs, status and events](#logs-status-and-events)
- [Databases: Postgres and Redis](#databases-postgres-and-redis)
- [Persistent storage](#persistent-storage)
- [Hostnames, custom domains and access control](#hostnames-custom-domains-and-access-control)
- [One-off commands and shells](#one-off-commands-and-shells)
- [Running a Mac in a closet](#running-a-mac-in-a-closet)
- [Backups and moving to a new Mac](#backups-and-moving-to-a-new-mac)
- [Command reference](#command-reference)
- [Troubleshooting](#troubleshooting)
- [How it works](#how-it-works)
- [Development](#development)

---

## What you need

- **An Apple Silicon Mac on macOS 26 or later.** A Mac mini is ideal; 16 GB
  comfortably runs about 20 small apps plus a database.
- **A domain on Cloudflare** (the free plan is fine) for public URLs. Without
  one, Berth still works on your LAN, or privately over Tailscale.
- **A GitHub account.** Berth needs a token to clone private repos and
  register webhooks. Public repos work without one, deploying by polling.

Homebrew installs the rest: Apple's
[`container`](https://github.com/apple/container) runtime,
[Caddy](https://caddyserver.com) and
[cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/).

## Setting up

```bash
brew tap dps/berth https://github.com/dps/berth
brew install --HEAD berth
berth init --domain example.com
```

`berth init` takes about three minutes and walks you through:

1. **Checks** this Mac and installs anything missing (`container`, `caddy`,
   `cloudflared`).
2. **Starts Apple's container system.** The first run downloads a small Linux
   kernel.
3. **Connects Cloudflare.** A browser opens for `cloudflared tunnel login`;
   pick the zone for your domain. Berth creates a tunnel named `berth` and
   points `*.example.com` at it, so every future app gets its hostname with no
   DNS step.
4. **Optionally takes a Cloudflare API token**, which lets custom domains
   update live. Create one at
   [dash.cloudflare.com/profile/api-tokens](https://dash.cloudflare.com/profile/api-tokens)
   with *Account · Cloudflare Tunnel · Edit* and *Zone · DNS · Edit* (add
   *Zone · WAF · Edit* if you'll use `--cf-ip-filter`). You can skip this and
   add it later.
5. **Gets a GitHub token**, in this order: `--github-token`, then your
   logged-in GitHub CLI (`gh auth token`), then a token you paste. A classic
   token with the `repo` and `admin:repo_hook` scopes works;
   [create one here](https://github.com/settings/tokens/new?scopes=repo,admin:repo_hook&description=berth).
6. **Installs three background services** (launchd agents): `berthd`, Caddy
   and cloudflared. They start at login and restart if they die.
7. **Asks before the two things that need `sudo`**: keeping the Mac awake and
   restarting it after power loss (`pmset`), and trusting Caddy's local
   certificate authority so `https://*.local` shows a lock in Safari.
8. **Explains the FileVault trade-off** (see
   [Reboots and FileVault](#reboots-and-filevault)) and prints a health check.

`init` is safe to re-run. It keeps your existing settings and changes only
what the flags say, for example to add a Cloudflare token later:

```bash
berth init --cf-token <token>
```

### Setup variations

| You want | Run |
| --- | --- |
| LAN only, no Cloudflare | `berth init --no-tunnel` |
| Private access from anywhere via Tailscale | `berth init --no-tunnel --tailscale` |
| Fully scripted, no prompts | `berth init --domain example.com --github-token ghp_… --cf-token … --yes` |
| Only GitHub's servers may reach the webhook URL | `berth init --cf-ip-filter` (needs the WAF permission on your Cloudflare token) |
| Recover from power cuts with nobody present | `berth init --unattended` (see [Reboots and FileVault](#reboots-and-filevault)) |
| Public repos only, no GitHub token | `berth init --skip-github` |

`--yes` never runs `sudo`; do the power settings yourself with
`sudo pmset -a sleep 0 disksleep 0 womp 1 autorestart 1`.

## Deploying an app

```bash
berth create myapp --repo you/myapp
```

This registers a webhook on the repo, gives the app its hostnames and deploys
the `main` branch. From then on, every push to `main` deploys.

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

The same contract as Heroku and Dokku, so code written for them (or by a
coding agent) works unchanged:

1. Listen on `$PORT` (8080 unless you set it) on `0.0.0.0`.
2. Read configuration from environment variables.
3. Write only to `/tmp` or a [mounted folder](#persistent-storage); everything
   else is reset on each deploy.
4. Log to stdout and stderr.

### Dockerfiles, or none

If the repo has a `Dockerfile` (or `Containerfile`) at its root, Berth builds
it. To use one elsewhere: `berth config:set myapp BERTH_DOCKERFILE=deploy/Dockerfile`.

Otherwise Berth detects the stack and generates one:

| Found in the repo | Stack | Starts with |
| --- | --- | --- |
| `bun.lock` / `bun.lockb` | Bun | `bun run start`, or your entry file |
| `package.json` | Node (npm, pnpm or yarn by lockfile; version from `engines.node`) | the `start` script, or `server.js` / `index.js` |
| `pyproject.toml` / `requirements.txt` | Python, installed with uv | uvicorn, gunicorn or `flask run` for your FastAPI/Flask/Starlette app; gunicorn for Django; else `python main.py` |
| `go.mod` | Go (multi-stage, small Alpine image) | the binary from `.` or `./cmd/<name>` |
| `Cargo.toml` | Rust (multi-stage) | the package's binary |
| `index.html` | Static site, served by Caddy | — |

A `build` script in `package.json` runs during the build. To see, or take
over, what Berth generates:

```bash
berth dockerfile myapp > Dockerfile    # commit it and edit freely
```

### Branches, staging and monorepos

```bash
berth create myapp-staging --repo you/myapp --branch staging     # a second app per branch
berth create api --repo you/mono --path services/api             # build from a subdirectory
```

A monorepo app deploys only when a push changes files under its path.

### How deploys are triggered

| Trigger | When it applies |
| --- | --- |
| **GitHub webhook** | Pushes deploy within seconds. Set up by `berth create` when you have a tunnel and a GitHub token. |
| **Polling** | Every 60 s Berth checks the branch head. Always on, so it covers missed webhooks, `--no-webhook`, `--no-tunnel` and repos outside GitHub. |
| **`git push berth main`** | Push straight to the Mac over SSH, Dokku-style, with the build streamed back to your terminal. |
| **`berth deploy myapp`** | Deploy now; add `--ref <sha|branch|tag>` for a specific commit, or `--no-cache` for a clean build. |

`--repo` also accepts any git URL (`https://gitlab.com/you/app.git`) or a
local path. Leave it out entirely for an app you only push to:

```bash
berth create myapp                 # no repo; deploy by pushing
cd ~/code/myapp && berth git:remote myapp
git push berth main
```

`git push berth` needs **Remote Login** on the Mac (System Settings → General →
Sharing). From outside your LAN, use `berth git:remote myapp --host <tailscale-name>`.

### Private repos

The GitHub token from `init` covers private repos. To use a read-only SSH
deploy key for one repo instead:

```bash
berth app:set-key myapp     # generates a key and adds it to the repo (or prints it to add)
```

## Releases, rollbacks and restarts

Every deploy creates a numbered **release**: an image plus the configuration
it started with.

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

**Zero-downtime deploys.** The new release starts next to the old one, and
Berth waits until it answers on `/` (any status below 500 counts). Only then
does traffic move, and the old release stops after a 5-second drain. If the
new release never becomes healthy, the old one keeps serving, the release is
marked failed, and you get its last 50 log lines.

```bash
berth rollback myapp          # back to the previous good release, in about 2 seconds
berth rollback myapp 2        # back to v2 specifically
berth restart myapp           # fresh container for the current release, zero downtime
berth stop myapp              # stop it; the URL answers 503
berth start myapp
```

A rollback restores that release's code *and* its configuration. Berth keeps
the images of the last 5 releases; older ones are garbage-collected and can be
redeployed by commit with `berth deploy myapp --ref <sha>`.

## Configuration

Configuration is environment variables. Changing them makes a new release from
the current image, with no rebuild, in a few seconds.

```bash
berth config:set myapp OPENAI_API_KEY=sk-… GREETING=hello
berth config:unset myapp GREETING
berth config:get myapp OPENAI_API_KEY
berth config:show myapp                 # secrets masked; --reveal to show, --shell for KEY=value
berth config:import myapp .env          # add a .env file; --replace to drop everything else
berth config:set myapp FOO=bar --no-restart     # save now, apply on the next deploy
```

`config:show` masks anything ending in `_KEY`, `_SECRET`, `_TOKEN` or
`PASSWORD`, plus `DATABASE_URL` and `REDIS_URL`.

Variables are given to the app at run time, never at build time. If your build
genuinely needs one (for example a public API URL baked into a frontend), mark
it:

```bash
berth config:set myapp NEXT_PUBLIC_API_URL=https://api.example.com --build-arg
```

### Berth settings

Variables starting with `BERTH_` tune Berth itself and are not passed to your app.

| Variable | Default | Meaning |
| --- | --- | --- |
| `BERTH_HEALTHCHECK` | `/` | path checked before traffic moves to a new release |
| `BERTH_HEALTH_TIMEOUT` | `60` | seconds to wait for it to become healthy |
| `BERTH_DRAIN` | `5` | seconds the old release keeps running after the switch |
| `BERTH_MEMORY` | `512M` | memory limit (`256M`, `1G`, …) |
| `BERTH_CPUS` | `1` | CPU limit |
| `BERTH_KEEP_RELEASES` | `5` | release images kept for rollback |
| `BERTH_DOCKERFILE` | — | Dockerfile path within the repo (or the `--path` subdirectory) |
| `PORT` | `8080` | the port your app listens on inside its container |

## Logs, status and events

```bash
berth logs myapp                   # last 200 lines
berth logs myapp -f                # follow
berth logs myapp --since 30m -n 1000
berth logs myapp --build           # the latest build's output (or --build 12)
berth logs myapp --access          # one line per HTTP request
berth logs --all -f                # every app, prefixed with its name
```

Each line is stamped with the time and the release that wrote it. Logs live in
`~/.berth/apps/<app>/logs/`, outlive containers, and are rotated at 50 MB.

```bash
berth status                       # everything on one screen
berth status myapp                 # one app in detail
```
```
berth v0.1.0 · example.com · berthd up 3d4h
container ✓ running · caddy ✓ up · tunnel ✓ connected (remote) · disk 101.7 GB free · memory 2.2 GB of 16.0 GB committed

APP     STATE    REL  SHA      UPTIME  MEM            PORT  URL                        LAST DEPLOY
myapp   running  v4   b3ab439  2h      131.0 MB/512M  8001  https://myapp.example.com  v4 running 2h ago
blog    running  v12  77ac1f0  3d      58.8 MB/512M   8003  https://blog.example.com   v12 running 3d ago

SERVICE      IMAGE        STATE    MEM       ENDPOINT           LINKED TO  LAST BACKUP
pg:db        postgres:17  running  209.3 MB  192.168.66.1:6001  [myapp]    9h ago
```

Warnings appear underneath: a crashed app, a service that's down, low disk, or
committed memory above 80% of the Mac's.

```bash
berth events -f       # deploys, crashes, restarts, webhook deliveries, backups, as they happen
```

## Databases: Postgres and Redis

```bash
berth pg:create db                 # Postgres 17 in its own VM
berth pg:link db myapp             # creates a role for myapp, sets DATABASE_URL, redeploys
```

Your app now has `DATABASE_URL=postgres://myapp:…@192.168.66.1:6001/db`.
Each linked app gets its own login, but they share ownership of the tables, so
several apps can use one database.

```bash
berth pg:psql db                                # interactive psql
berth pg:psql db -c "select count(*) from users"
berth pg:backup db                              # pg_dump → ~/.berth/backups/pg/db/<time>.dump
berth pg:restore db ~/.berth/backups/pg/db/20261025-030000.dump
berth pg:expose db 15432                        # reach it from TablePlus etc. at 127.0.0.1:15432
berth pg:expose db off
berth pg:upgrade db --to 18                     # dump, new Postgres, restore, relink
berth pg:unlink db myapp                        # revoke the login, remove DATABASE_URL
berth pg:destroy db                             # refuses while apps are linked (--force to override)
berth pg:list
```

**Every Postgres database is dumped nightly** at 03:00 and the last 14 dumps
are kept in `~/.berth/backups/pg/`, where Time Machine picks them up. The dumps
are the real backup: the live database files sit inside a `container` volume,
and a file copy of a running database isn't reliable anyway. `pg:upgrade` keeps
the old data volume until you delete it.

Linking a second database to the same app names its variable after it
(`PG_ANALYTICS_URL`); choose your own with `--as`.

**Redis** works the same way, with `REDIS_URL` and append-only persistence:

```bash
berth redis:create cache
berth redis:link cache myapp
berth redis:cli cache GET somekey
berth redis:backup cache
berth redis:restore cache ~/.berth/backups/redis/cache/<time>.rdb
```

`pg:create` and `redis:create` take `--memory` (defaults `512M` and `256M`)
and `--image` (for example `--image postgres:16`).

## Persistent storage

Container filesystems are reset on every deploy. For SQLite files, uploads or
caches, mount a folder that persists:

```bash
berth storage:mount myapp /data         # then e.g. DATABASE_PATH=/data/app.db
berth storage:list myapp
berth storage:unmount myapp /data       # stops mounting it; the files stay
```

The folder lives at `~/.berth/data/apps/myapp/data/`: an ordinary folder on
your Mac, included in Time Machine and in `berth backup`.

## Hostnames, custom domains and access control

Every app is reachable at:

| URL | From |
| --- | --- |
| `https://myapp.example.com` | anywhere, through the Cloudflare tunnel |
| `https://myapp.your-mac.local` | your LAN; advertised over Bonjour, with Caddy's local certificate |
| `https://myapp.<tailnet>.ts.net` | your tailnet, with `init --tailscale` |

`your-mac` is the Mac's local hostname (System Settings → General → Sharing).
To make LAN URLs show a lock on this Mac, run `berth trust-ca`. On other
devices, install `~/.berth/caddy/pki/authorities/local/root.crt` as a trusted
certificate.

### Custom domains

```bash
berth domains:add myapp www.mysite.com
berth domains:list myapp
berth domains:remove myapp www.mysite.com
```

If the domain is on your Cloudflare account and you gave `init` an API token,
Berth creates the DNS record and updates the tunnel live. Otherwise it prints
the CNAME to create (`www.mysite.com → <tunnel-id>.cfargotunnel.com`, proxied).
Without a token it also has to restart cloudflared, which drops in-flight
requests for a moment.

### Private apps and passwords

```bash
berth create admin-tool --repo you/admin-tool --private   # LAN and Tailscale only; no public URL
berth auth:set myapp alice:correct-horse-battery          # HTTP basic auth, enforced by Caddy
berth auth:unset myapp
```

## One-off commands and shells

```bash
berth run myapp npx prisma migrate deploy      # fresh container: current image, current config
berth run myapp python manage.py createsuperuser
berth exec myapp                               # shell inside the running container
berth exec myapp node -e 'console.log(process.env.PORT)'
```

`run` exits with the command's exit code, so it works in scripts.

## Running a Mac in a closet

Berth is built to run for months with no one at the screen.

- **Crashes heal themselves.** An app that exits is restarted with backoff
  (1 s up to 60 s). After 10 failures in a row it's marked `crashed` and left
  alone until you deploy or `berth restart` it. Databases are restarted
  whenever they're down.
- **Reboots heal themselves.** When `berthd` starts, it starts Apple's
  container system, the databases, then every app, oldest first.
- **Upgrades are hands-off.** `brew upgrade berth` replaces the binaries; the
  running `berthd` notices, waits for any deploy in progress, and restarts
  itself. Apps keep running throughout.

### Notifications

```bash
berth notify:set https://hooks.slack.com/services/…    # Slack, Discord or ntfy.sh URLs
berth notify:set off
```

You're notified when a deploy succeeds or fails, when an app is stuck
crash-looping, and when a nightly backup fails. `notify:set` sends a test
message first.

### Reboots and FileVault

macOS turns off automatic login while FileVault is on, and after a power cut a
FileVault Mac waits at a password screen before anything, Berth included, can
start. So choose:

- **FileVault on (the default, recommended).** Crashes and restarts all
  recover on their own. For planned reboots, such as macOS updates, use
  `berth reboot`: it unlocks the disk for exactly one boot
  (`fdesetup authrestart`), so the Mac comes back with no one present. After
  an *unexpected* power cut, someone has to type the password. A small UPS
  makes that rare.
- **Unattended (`berth init --unattended`).** Turn FileVault off and automatic
  login on (`init` shows you where). Power cuts recover with no one present,
  but the disk, including your secrets, is unencrypted.

`berth doctor` tells you which mode you're in and whether it's set up right.

### Keeping it healthy

```bash
berth doctor             # checks everything Berth relies on; prints a fix for each problem
berth doctor --images    # also: which base images have updates
berth gc                 # remove old release images, expired dumps, leftover build files
```

Berth refuses to deploy when less than 2 GB of disk is free, rather than
filling the boot volume.

## Backups and moving to a new Mac

```bash
berth backup                         # → ~/berth-backup-<time>.tar.gz
berth backup /Volumes/USB/berth.tar.gz
```

The archive holds everything needed to rebuild: app settings and config,
release history, fresh database dumps and your storage folders. It leaves out
anything that can be re-fetched or rebuilt (git mirrors, images, logs). It
contains your secrets, so keep it safe.

To restore, on the same Mac or a new one:

```bash
berth init --domain example.com      # on a Mac with no apps yet
berth restore berth-backup.tar.gz
```

Berth recreates the databases from their dumps, restores the storage folders,
and redeploys every app from its repository. Apps created without a repo come
back with their settings; push to them to deploy.

### Uninstalling

```bash
berth uninstall              # stop everything and remove the background services; keeps ~/.berth
berth uninstall --purge      # also delete every app, webhook, database volume, image and ~/.berth
```

## Command reference

| Command | What it does |
| --- | --- |
| **Setup** | |
| `berth init --domain d` | one-time setup; safe to re-run (flags above) |
| `berth doctor [--images]` | check everything and print fixes |
| `berth trust-ca` | trust Caddy's local CA in your login keychain |
| `berth uninstall [--purge]` | remove Berth |
| **Apps** | |
| `berth create <app> [--repo r] [--branch b] [--path p] [--private] [--no-webhook] [--no-deploy]` | register and deploy an app |
| `berth deploy <app> [--ref r] [--no-cache]` | build and release now |
| `berth rollback <app> [n]` | back to the previous (or nth) release |
| `berth restart \| stop \| start <app>` | lifecycle |
| `berth releases <app>` | release history |
| `berth destroy <app> [--keep-data]` | delete an app, its webhook and images |
| `berth dockerfile <app>` | print the Dockerfile Berth would use |
| `berth git:remote <app> [--host h]` | add a `berth` remote for `git push berth` |
| `berth app:set-key <app>` | use an SSH deploy key for this repo |
| **Config** | |
| `berth config:set <app> K=v … [--no-restart] [--build-arg]` | set variables and redeploy |
| `berth config:unset <app> K …` | remove variables |
| `berth config:get <app> K` | print one variable |
| `berth config:show <app> [--reveal] [--shell]` | list variables |
| `berth config:import <app> <file> [--replace]` | load a `.env` file |
| **Logs** | |
| `berth logs <app> [-f] [-n N] [--since d] [--build [n]] [--access]` | app, build or access logs |
| `berth logs --all [-f]` | every app's logs |
| `berth status [app]` | overview |
| `berth events [-f]` | event log |
| **Hostnames and access** | |
| `berth domains:add \| remove \| list <app> [host]` | custom domains |
| `berth auth:set <app> user:pass` · `berth auth:unset <app>` | basic auth |
| **Storage and services** | |
| `berth storage:mount \| unmount \| list <app> [path]` | persistent folders |
| `berth pg:create \| link \| unlink \| psql \| backup \| restore \| expose \| upgrade \| destroy \| list` | Postgres |
| `berth redis:create \| link \| unlink \| cli \| backup \| restore \| expose \| destroy \| list` | Redis |
| **Running things** | |
| `berth run <app> <cmd…>` | one-off command in a fresh container |
| `berth exec <app> [cmd…]` | command or shell in the running container |
| **Operations** | |
| `berth gc` | clean up old images, dumps and build files |
| `berth backup [file]` · `berth restore <file>` | archive and restore everything |
| `berth notify:set <url\|off>` | deploy and crash notifications |
| `berth reboot` | planned reboot that comes back unattended |
| `berth reload` | re-apply `~/.berth` after editing files by hand |
| `berth completion zsh \| bash \| fish` | shell completions (installed by Homebrew) |

Add `--json` to any non-interactive command for machine-readable output.
Progress goes to stderr and the result to stdout, which suits scripts and
coding agents.

## Troubleshooting

**Start with `berth doctor`.** It checks each moving part and prints the
command that fixes it.

**A deploy failed.** The error ends with the app's last log lines. For the
full story: `berth logs myapp --build`, then `berth logs myapp`. The previous
release is still serving. Common causes: the app listens on `localhost`
instead of `0.0.0.0`, or ignores `$PORT`; the app is slow to boot (raise
`BERTH_HEALTH_TIMEOUT`); `/` returns a 5xx (point `BERTH_HEALTHCHECK` at a
health endpoint).

**A push didn't deploy.** Run `berth events`. You'll see `webhook` entries if
GitHub reached the Mac, or `webhook.rejected` for a bad signature. Without
webhooks, polling picks the push up within 60 s. A monorepo app ignores pushes
that don't touch its path.

**`berth: berthd is not running`.** Start it with
`launchctl kickstart -k gui/$UID/dev.berth.berthd`; its log is
`~/.berth/logs/berthd.log`.

**The public URL doesn't work but the `.local` one does.** That's the tunnel:
`berth doctor` checks that it's connected and that the wildcard DNS record
resolves. cloudflared's log is `~/.berth/logs/cloudflared.log`.

**An app shows `crashed`.** It failed to start 10 times in a row. Check
`berth logs myapp`, fix it, then `berth restart myapp` or push.

**I edited files in `~/.berth` by hand.** Run `berth reload`. `berthd` rebuilds
the proxy and tunnel configuration and the set of running containers from the
files.

### Environment variables

| Variable | Use |
| --- | --- |
| `BERTH_HOME` | use a different state directory (default `~/.berth`) |
| `BERTH_DEBUG=1` | `berthd` logs every external command with its duration |
| `BERTH_GITHUB_CLIENT_ID` | GitHub OAuth app for device-flow login in `init` |
| `BERTH_CONTAINER_BIN` | path to the `container` binary |
| `BERTH_NO_MDNS=1` | don't advertise `.local` hostnames |

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

- **`berthd`** is the brain: it receives webhooks, builds with `container
  build`, runs each release in its own micro-VM, health-checks it, switches
  Caddy over, and watches everything afterwards. It's the only thing that
  writes to `~/.berth`.
- **Caddy** routes each hostname to the right app, terminates LAN HTTPS,
  compresses responses, writes access logs and enforces basic auth.
- **cloudflared** keeps an outbound connection to Cloudflare, so the Mac needs
  no open ports. TLS for public URLs ends at Cloudflare's edge.
- **Each app and database is its own lightweight VM** with its own Linux
  kernel, so a compromised app can't reach the others or macOS.

All state is plain files you can read:

```
~/.berth/
  config.json              domain, ports, defaults
  apps/<app>/app.json      repo, branch, mounts, links, domains
  apps/<app>/env           live config (mode 0600)
  apps/<app>/releases/     one JSON file per release
  apps/<app>/logs/         app.log, build-<n>.log, access.log
  services/<kind>/<name>/  database settings
  backups/                 database dumps
  data/apps/<app>/         storage:mount folders
  secrets/                 tokens and tunnel credentials (mode 0700)
  logs/                    berthd, Caddy and cloudflared logs
  events.log               the event log
```

The full design, including what changed while building it, is in
[design.md](design.md).

## Development

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

## License

MIT
