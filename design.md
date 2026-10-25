# Berth: a Dokku for Mac minis — design doc

Oct 24, 2026 · @David Singleton

## Summary

Berth is a small PaaS for Apple Silicon Mac minis: `git push` to GitHub and your app is live on the internet at `app.yourdomain.com` in under a minute, with a Postgres database one command away. It composes four things that already exist and are each very good at one job — Apple's `container` CLI (one micro-VM per app), Caddy (routing), Cloudflare Tunnel (public ingress with no open ports), and GitHub webhooks (deploy trigger) — behind a Dokku-shaped CLI. The whole system lives in `~/.berth`, needs no `sudo` after install, no YAML, and no Docker Desktop.

Target: macOS 26, Apple Silicon. Language: Go (two static binaries, `berth` and `berthd`; same toolchain Caddy and cloudflared use). License: MIT. Name: *Berth* (where a ship ties up); code at github.com/dps/berth.

**Status (Oct 2026):** v1 is implemented as described here and smoke-tested against Apple's `container` 1.6.1 on macOS 26.1. Not yet exercised end to end: launchd agents, the Cloudflare tunnel, GitHub webhook registration and Tailscale mode (see Open questions). Sections below describe what the code does; findings from building it are written in place.

```bash
brew tap dps/berth https://github.com/dps/berth && brew install berth   # once
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
- Buildpack compatibility, Procfile multi-process types (v1 runs one `web` process per app; `worker` is v2).
- Intel Macs or macOS < 26 — Apple's `container` is Apple Silicon only and effectively requires macOS 26.
- Replacing `container`, Caddy or cloudflared. Berth orchestrates them; it does not reimplement them.

## The developer experience

The whole product is three moments. Everything in later sections exists to make these three short.

**1. Install (once, \~3 minutes)**

```bash
brew tap dps/berth https://github.com/dps/berth   # the formula lives in this repo
brew install berth                                 # --HEAD until a release is tagged
berth init --domain example.com
```

`berth init` walks through: check Apple Silicon + macOS 26 + `container` installed (offers `brew install container` if not; it is a Homebrew formula, not a cask); `container system start`; install Caddy and cloudflared via Homebrew if missing; log in to Cloudflare in the browser (`cloudflared tunnel login`, run with a temporary `HOME` so it never touches `~/.cloudflared`; the user picks the zone and Berth checks it is the right one); create a tunnel named `berth` and route `*.example.com` to it; log in to GitHub in the browser with the device flow (the one-time code is copied to the clipboard and the verification page opened) to get a token for cloning private repos and registering webhooks, falling back to `--github-token`, `gh auth token`, or a pasted classic token; install `berthd`, Caddy and cloudflared as launchd agents; offer the `pmset` change and the Caddy CA trust; explain the FileVault/reboot choice; print the `berth doctor` summary. `init` is idempotent: re-running it keeps existing settings and only changes what the flags say. Flags: `--no-tunnel` (LAN only), `--tailscale`, `--cf-ip-filter`, `--unattended`, `--github-token`/`--cf-token`, `--tunnel-name`, `--skip-github`, `--no-launchd` (run `caddy` and `berthd` yourself) and `--yes` (non-interactive: no prompts, no `sudo` steps).

**2. First deploy (per app, \~1 minute)**

```bash
berth create myapp --repo me/myapp
# → registered webhook on github.com/me/myapp
# → https://myapp.example.com (DNS live in ~30s)
# → building main@a1b2c3d ... ✓ running
```

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

Why a daemon rather than a Dokku-style git hook: GitHub is the source of truth for vibe-coded repos (the agent pushes there), the Mac is behind NAT, and a long-lived process is what you need anyway for health checks and restarts. `berthd` is a single Go process (the `daemon` package is about 2.8k lines; the whole codebase about 10.5k plus 1.1k of tests), no database — state is JSON files and the filesystem.

## Apps, builds and releases

An **app** is a name, a repo (GitHub `owner/name`, any git URL or local path, or none for push-only apps), a branch (default `main`), an env map, and a list of releases. A **release** is an immutable image tagged `berth/<app>:<n>` plus the env snapshot it was started with; `current` points at one of them. Deploy = make a new release, start it, swap traffic, stop the old one. Rollback = start the older release's image with its own env snapshot and do the same swap; `current` then points at it. A config change (`config:set`, `pg:link`, `storage:mount`) makes a new release that reuses the current image with the new env, so it takes seconds and never rebuilds; only `--build-arg` changes force a build.

**Build pipeline** (`berthd`, one deploy at a time per app, concurrent across apps):

1. Fetch: `git fetch` the branch into `refs/remotes/origin/*` of the bare mirror `~/.berth/apps/<app>/repo.git` (remote refs never overwrite `refs/heads`, so branches pushed straight to the Mac survive), then `git archive` the commit (or `<sha>:<path>` for monorepos) into `~/.berth/apps/<app>/build/<sha>-<n>/`. If the fetch fails but the ref exists locally, the deploy continues from the local copy.
2. Choose a Dockerfile: the repo's own if present (path overridable with `berth config:set myapp BERTH_DOCKERFILE=deploy/Dockerfile`), else a template picked by detection order: `bun.lockb`/`bun.lock` → Bun; `package.json` → Node (respects `engines.node`, uses npm, pnpm or yarn by lockfile, runs the `build` script if present, starts with the `start` script or `server.js`/`index.js`); `pyproject.toml`/`requirements.txt` → Python (uv; finds the FastAPI/Flask/Starlette app object and starts it with `uvicorn`, `gunicorn` or `flask run` depending on the dependencies; Django via `gunicorn <project>.wsgi`; else `python main.py`); `go.mod` → Go multi-stage (`./cmd/<name>` if there is no root `main.go`, Alpine runtime so `berth exec` has a shell); `Cargo.toml` → Rust; `index.html` with no manifest → static (Caddy file server image). Detection is one small package (`internal/detect`); every template is a plain file under `templates/` so users and agents can read and copy them. A generated Dockerfile is written into the build directory as `.berth.Dockerfile`.
3. Build: `container build --progress plain -t berth/<app>:<n> -f <Dockerfile> <dir>`; the builder VM's layer cache persists across builds, and so do BuildKit cache mounts, which the generated Dockerfiles use for package-manager downloads, the Go build cache and Rust's `target/` (per app). A code change to the Rust demo redeploys in 4 s instead of recompiling axum and tokio. (`container build -q` hung indefinitely twice on 1.6.1; Berth always uses `--progress plain`.) Build logs stream to the CLI and to `~/.berth/apps/<app>/logs/build-<n>.log`.
4. Start: `container run -d --name berth-<app>-<n>-<suffix> --network berth -p 127.0.0.1:<slot>:<PORT> --env-file releases/<n>.env --cpus/--memory from config -l dev.berth.app=<app> --init berth/<app>:<n>`. The suffix makes every start unique, so a release can be restarted next to its own running container. `PORT` is injected (default 8080) and `BERTH_*` settings are not passed to the app; the host port is whichever of the app's two slots (allocated from 8001–8999 at the first deploy) is free, so two releases of one app run side by side. Multi-line env values go as `-e` because `--env-file` takes values literally.
5. Health: poll `http://127.0.0.1:<port><healthcheck path>` (default `/`, any 2xx–4xx counts as up, configurable with `BERTH_HEALTHCHECK` and `BERTH_HEALTH_TIMEOUT`, default 60 s). The check fails fast if the container exits. On failure: remove the new container, keep the old one serving, mark the release failed, exit non-zero with the last 50 log lines.
6. Swap: point `current` at the new release; in the background berthd waits `BERTH_DRAIN` seconds (default 5) and stops the previous container. **Deploys never touch Caddy's config**, because every admin-API change, even a one-field PATCH, is a full graceful reload, and reloads drop requests at the margins. The first version reloaded per deploy and lost 4 of 5,172 requests over 20 deploys locally (idle keep-alive connections closed as they were reused; `Connection: close` on the tunnel side fixed that), then 1 of 1,161 through the real tunnel (a fresh cloudflared connection accepted during the listener hand-over). Instead, each app's route uses Caddy's SRV dynamic upstreams: Caddy looks up `_<app>._tcp.berth.internal` every second from a small DNS responder in berthd (`127.0.0.1:7753`, `internal/daemon/dns.go`), which answers with the current release's port; `grace_period: 1h` keeps the last answer if berthd restarts. Moving `current` moves traffic within a second, while the old release finishes its in-flight requests during the drain. (An intermediate design listed both of an app's ports with Caddy's `first` policy; whenever the old release held the preferred port it kept taking new requests for the whole drain, so "v3 is live" was untrue for 5 s in half of all deploys. The Chirp demo showed it: posts went to the old, in-memory release.) Each app still owns two fixed ports (*slots*) that releases alternate between, and a deploy that needs a slot still held by a draining release ends that drain early. Upstream keep-alive is off, and refused connections are retried for 5 s. Results: the new release serves 0.04 s after `config:set` returns, and 0 failures in 1,430 requests through the real tunnel across 20 deploys and a restart.
7. GC: keep the last 5 releases' images (`BERTH_KEEP_RELEASES`), delete older images, their env snapshots and build logs, and mark those releases `pruned` (their metadata stays for `berth releases`; rollback to them is refused with the `berth deploy --ref` to use instead). Stopped containers are always removed, never kept. The build directory is deleted after every build.

**Contract with the app**, deliberately identical to Heroku/Dokku so agent-written code works unchanged: listen on `$PORT` on `0.0.0.0`; read config from env; write only to `/tmp` or a mounted volume; log to stdout. `berth create` prints these four lines.

**App names** are DNS labels (`[a-z0-9-]`, ≤ 63 chars) and must not collide with Berth's own hostnames: `hooks`, `berth`, `www`, and anything starting with `pg-`/`redis-` (container names on the `berth` network) are reserved, and `berth create` rejects them with a one-line error.

**Process model, v1**: one container, one process, per app. Workers, cron and multiple process types are v2 (`Procfile` support, `berth ps:scale`), kept out so v1 ships. A second "app" pointing at the same repo with a different start command covers most worker needs meanwhile.

## Deploy triggers

GitHub webhook is the primary trigger; polling is the no-config fallback; `git push berth` and `berth deploy` exist for people who want Dokku's exact feel or are offline. All four end in the same `berthd` deploy job.

| Trigger | How it reaches berthd | Latency | Setup |
| --- | --- | --- | --- |
| GitHub webhook | `POST https://hooks.<domain>/github/<app>` through the tunnel; HMAC-SHA256 checked against a per-app secret | \~2 s | automatic: `berth create --repo` registers it with the GitHub token from `init` |
| Polling | every 60 s for every app with a repo: GitHub repos via the API (`/commits/<branch>` with ETag, so nearly free), other git URLs via `git ls-remote` | ≤ 60 s | always on, so it is the fallback when the token lacks webhook rights, with `--no-webhook` or `--no-tunnel`, and the safety net if a webhook is missed |
| `git push berth main` | a `berth` git remote whose URL is `ssh://<user>@<mac>.local/<home>/.berth/apps/<app>/repo.git`; its `post-receive` hook runs `berth deploy <app> --ref <sha>`, so the build streams back to the pusher | \~1 s | `berth git:remote myapp` adds the remote (`--host` for a Tailscale name); needs Remote Login on the Mac. Apps created without `--repo` deploy only this way or with `berth deploy` |
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

**Caddy (routing).** One Caddy, configured entirely through its admin API so berthd never rewrites a Caddyfile. Caddy listens on `127.0.0.1:7780` for the tunnel rather than `:80`: macOS lets unprivileged processes bind ports below 1024 only on the wildcard address, so `127.0.0.1:80` needs root while `:443` and `:80` on all interfaces do not. Each app is a route: `host == <app>.<domain> or <app>.<hostname>.local → reverse_proxy 127.0.0.1:<port>`. A `--private` app's route matches only the `.local` (and Tailscale) hostname; since the wildcard tunnel ingress still delivers `<app>.<domain>` to Caddy, that request falls through to Caddy's catch-all 404. Privacy is enforced in Caddy, not in the tunnel. Deploys regenerate and load the whole config (see the swap step above). Caddy also provides: gzip/zstd, HTTP→HTTPS on LAN, access logs per app (`~/.berth/apps/<app>/logs/access.log`, JSON), a `berth.<domain>` status route (v2), and basic auth for an app with `berth auth:set myapp user:pass`. Caddy's internal CA issues LAN certificates; `berth init` (or `berth trust-ca`) offers to trust the root in the login keychain so Safari on the LAN shows a lock. macOS only advertises the bare `<hostname>.local` over Bonjour, so berthd registers each `<app>.<hostname>.local` with `dns-sd -P` (one small process per app, re-registered if the LAN address changes); `<hostname>` is the Mac's `LocalHostName`.

**`container` network (app ↔ service).** All Berth containers join a user-defined network `berth` (`container network create berth`). Container names do not resolve between containers unless an administrator creates a local DNS domain (`sudo container system dns create`), container IPs change on every restart, and single files cannot be bind-mounted (so no managed `/etc/hosts`). Instead each service publishes its port on the network's gateway address (for example `192.168.66.1:6001`), which every container on the network can reach and the LAN cannot; a linked Postgres is `postgres://myapp:<pw>@192.168.66.1:6001/mydb`. berthd records the gateway and, if the network is ever recreated with a new one, rewrites link URLs and redeploys the affected apps. Apps publish their web port only to `127.0.0.1`; nothing inside a container is reachable from the LAN except through Caddy.

**No-tunnel mode** (`berth init --no-tunnel`): LAN hostnames only, or pair with Tailscale for private-but-remote access. `berth init --tailscale` reads the tailnet's MagicDNS suffix, and berthd advertises each app as a Tailscale Service (`tailscale serve --service=svc:<app> --https=443 --bg http://127.0.0.1:7780`), so it is reachable as `https://<app>.<tailnet>.ts.net`; Caddy routes that hostname like any other. Each service must be approved once in the Tailscale admin console (or auto-approved by tag).

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

The app receives `X-Berth-User-Id` (`google:<sub>`, stable for life), `X-Berth-Email`, `X-Berth-Name`, `X-Berth-Picture`, and `X-Berth-Identity`: a 5-minute EdDSA JWT (audience = the hostname) for apps that want to verify instead of trusting the proxy; the public key is at `/_berth/jwks.json`. App ports are published on `127.0.0.1` only, so Caddy is the only way in from the network. Every app also gets `/_berth/login?return=/path`, `/_berth/logout` (POST, same-origin only; GET shows a confirm button), and `/_berth/me` (the identity as JSON, for single-page apps). Locally (`npm run dev`, no Berth) the headers are absent and the app sees an anonymous visitor.

**Login.** Google needs a fixed list of redirect URIs and only accepts public hostnames, so there is one sign-in host, `auth.<domain>` by default (already covered by the wildcard DNS record, tunnel rule and Cloudflare certificate). `auth:setup --host signin.<domain>` picks another name directly under the domain; setup refuses a name that is an app or has its own DNS record (the first real install had `auth.cronbon.com` pointing at another server), and `create` and `domains:add` refuse the sign-in host afterwards:

1. `chirp.<domain>/_berth/login?return=/compose` stores `{host, return}` under a random nonce, sets the nonce in a short-lived host-only cookie, and redirects to `auth.<domain>/start?host=chirp.<domain>&n=sha256(nonce)`.
2. `auth.<domain>/start` refuses hosts that aren't Berth apps. If the browser already has a session on the auth host, it skips Google. Otherwise it redirects to Google with `state`, an OIDC `nonce` and PKCE, remembering them under `state` (also set as a cookie on the auth host).
3. Google returns to `auth.<domain>/callback`. berthd checks `state` against the cookie, exchanges the code (with the PKCE verifier and client secret) and reads the ID token it got straight from Google's token endpoint over TLS: issuer, audience, expiry, nonce, and `email_verified` must all check out. It creates a session on the auth host, so the next app's sign-in is a redirect, not a Google prompt.
4. It issues a one-time code (60 s, bound to the app host and `sha256(nonce)`) and redirects to `chirp.<domain>/_berth/callback?code=…`. The app host consumes the code only if its own nonce cookie matches, which stops an attacker from logging a victim into the attacker's account, then sets `__Host-berth_session` (host-only, Secure, HttpOnly, SameSite=Lax) and redirects to `return`, which must be a path on the same host.

The hand-off works for custom domains in other zones and for LAN and Tailscale hostnames too, because only the browser carries the code. Sessions live in `~/.berth/auth/sessions.json` (only hashes of the cookie values), last 30 days and are extended on use; logging out of one app signs the person out of every app and the auth host. `berth auth:sessions` lists them and `berth auth:revoke <email>` ends them.

**Setup** is one command (`berth auth:setup google [--host h]`), because Google has no API for creating OAuth clients: `berth auth:setup google` prints the steps for the Google Cloud console (create a Web application client, add `https://auth.<domain>/callback` as the redirect URI, publish the consent screen; only the `openid email profile` scopes are requested, which need no Google verification) and asks for the client ID and secret. The ID goes in `~/.berth/auth/config.json`, the secret in `secrets/google.client_secret`, the session-signing key in `secrets/auth.key`. Visiting `https://auth.<domain>/` shows who is signed in, which makes a good first test.

Basic auth (`berth auth:set`) remains for sites where a password is simpler; it runs before the access check.

## Storage

Postgres is the built-in service; everything else is a bind-mounted folder. Both keep data on the Mac's own disk under `~/.berth`, so Time Machine (or any backup the user already has) covers it with no extra setup — via nightly dumps for Postgres, directly for app folders.

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

**Volumes for apps** (SQLite, uploads, caches):

```bash
berth storage:mount myapp /data   # → ~/.berth/data/apps/myapp/data, bind-mounted read-write
```

Stored in app state and added as `-v` on every release, so it survives deploys and rollbacks. Agents love SQLite; this makes `DATABASE_PATH=/data/app.db` just work.

**Backups and restore**: `berth backup` takes a fresh `pg_dump` of every database, then tars `~/.berth` minus images, build caches and live Postgres data (state, env, pg dumps, app volumes) into one archive; `berth restore <archive>`, run after `berth init` on a Mac with no apps, unpacks the archive (merging `config.json` so this Mac keeps its own hostname, ports and network gateway), creates fresh volumes, restores the latest Postgres dump and Redis RDB, re-creates roles, and redeploys every app from its repo; berthd's supervisor is paused meanwhile. Apps without a repo come back as config only and need a push. This is also the migration story between machines. The archive excludes git mirrors, build directories and logs.

**Disk**: `berth status` shows free disk; `berth gc` prunes old releases' images past the keep count, dumps past retention and build directories left by interrupted deploys. berthd refuses to deploy below 2 GB free rather than filling the boot volume.

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
  apps/<app>/
    app.json             # repo, branch, path, private, mounts, links, domains, auth, hook secret, poll state
    env                  # KEY=value, mode 0600; the live config
    releases/<n>.json    # sha, image, env snapshot, host port, container, status, timestamps
    releases/<n>.env     # the snapshot as an --env-file, mode 0600
    current              # → release number
    repo.git/            # bare mirror
    build/<sha>-<n>/     # exported tree, deleted after build
    logs/                # build-<n>.log, app.log (rotated), access.log
  services/<pg|redis>/<name>/
    service.json         # image, port, volume, password, per-app link roles
    env                  # the container's --env-file (passwords), mode 0600
  data/apps/<app>/<mount>/
  backups/pg/<name>/*.dump
  backups/redis/<name>/*.rdb
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
5. **Notifications** (shipped in v1). A webhook URL in `config.json` (`berth notify:set https://...`, which sends a test message) receives JSON on deploy success/failure, crash-loop and failed nightly backups. The JSON carries `text` (Slack) and `content` (Discord); URLs containing `ntfy` get a plain-text body with a title header instead. A macOS notification on the Mac itself is nearly free to add but nobody is looking at that screen.

**Reboots and FileVault.** macOS disables automatic login while FileVault is on, and a FileVault Mac stops at a pre-boot password screen after any power loss. Berth cannot run until someone types that password, so there is a real trade-off and `berth init` makes the user choose it explicitly:

- **FileVault on (default, recommended for a Mac that holds secrets).** Crashes and restarts of apps, berthd, Caddy or cloudflared all recover unattended. *Planned* reboots (`berth reboot`, used before macOS updates) run `sudo fdesetup authrestart`, which unlocks the disk for the next boot only; the FileVault unlock logs the user in, so the LaunchAgents start with no one at the screen. *Unplanned* power loss stops at the pre-boot password screen until someone types the password there (Screen Sharing cannot reach it). A small UPS is the practical answer. `berth doctor` reports this mode as a warning, not a failure.
- **FileVault off + auto-login (`berth init --unattended`).** Survives power cuts with no human; the disk is unencrypted at rest, so secrets in `~/.berth/secrets` and app `env` files are readable by anyone with physical access. `init` prints the exact System Settings steps (FileVault off, Automatic login on) instead of scripting them, because turning off disk encryption should be a deliberate act by the user.

Which mode is active is recorded in `config.json`, reported by `berth doctor`, and included in `berth status --json`.

**Updates**: `brew upgrade berth` replaces the binaries; berthd notices its binary changed (checked every 10 s), waits until no deploy is running, and re-executes itself in place; running apps are untouched. Caddy, cloudflared and `container` are updated by Homebrew independently; Berth records the `container` version it was tested against in `config.json` and `berth doctor` warns on a different one.

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
- **Input validation**: values that reach git, shell hooks, Caddy matchers or Cloudflare expressions are validated where they enter: branches, paths and refs follow git ref rules and can't start with `-`; repos can't start with `-` or contain shell metacharacters; custom domains must be plain hostnames (no wildcards) and can't collide with `hooks.<domain>`, another app's `<app>.<domain>` or LAN/tailnet names; the `git push berth` hook single-quotes every field anyway.
- **Supply chain**: images are pulled from the registry the Dockerfile names; Berth templates pin major tags (`node:22-slim`, `postgres:17`). `berth doctor --images` lists images with available updates.
- **Sign-in**: Google login runs in berthd behind Caddy's `forward_auth` (see Sign-in). Clients can't forge identity headers (Caddy deletes `X-Berth-*` on every route); one-time codes, nonce cookies and PKCE protect the login hand-off; redirects only go to Berth app hostnames and same-host paths; logout is POST with an Origin check; sessions are stored hashed. If berthd is down, apps with a policy answer 502, so `allow` and `signed-in` apps fail closed.
- **Public by default is a choice**: `berth create --private` creates the app with no public hostname: its Caddy route matches only LAN/Tailscale hosts, so tunnel requests for it get a 404; `berth auth:set` adds basic auth in Caddy; both are one flag because the hobbyist default should be safe to flip either way.

## CLI reference

Dokku's `noun:verb` style, with the handful of commands people type daily at the top level. Every non-interactive command accepts `--json` for agents and scripts (progress goes to stderr, the result to stdout). Column 3 is the Dokku equivalent, for muscle memory.

| Command | Does | Dokku |
| --- | --- | --- |
| `berth init --domain d [--no-tunnel\|--tailscale] [--cf-ip-filter] [--unattended] [--github-token t] [--cf-token t] [--no-launchd] [--yes]` | one-time setup of tunnel, Caddy, launchd, GitHub login | bootstrap.sh |
| `berth create <app> [--repo owner/name\|url\|path] [--branch b] [--path p] [--private] [--no-webhook] [--no-deploy]` | register app, webhook, hostname; first deploy | `apps:create` + git remote |
| `berth deploy <app> [--ref sha] [--no-cache]` | build + release now | `git push dokku` |
| `berth rollback <app> [n]` | swap to previous (or release n) | `ps:rebuild` / manual |
| `berth logs <app>\|--all [-f] [-n N] [--since 1h] [--build [n]] [--access]` | app, build, or access logs | `logs` |
| `berth status [app]` | everything at a glance | `ps:report` |
| `berth restart\|stop\|start <app>` | lifecycle (restart is zero-downtime; a stopped app answers 503) | `ps:restart` etc. |
| `berth destroy <app> [--keep-data] [--yes]` | remove app, hostname, webhook, images, link roles | `apps:destroy` |
| `berth config:set\|get\|unset\|show\|import <app> ...` | env vars | `config:*` |
| `berth domains:add\|remove\|list <app> <host>` | extra hostnames via tunnel ingress | `domains:*` |
| `berth auth:set\|unset <app> user:pass` | basic auth in Caddy | http-auth plugin |
| `berth access <app> [path] <public\|signed-in\|allow who...\|off> [--remove]` | Google sign-in policy for an app or path | — |
| `berth auth:setup google` / `auth:status` / `auth:group <name> [who...]` / `auth:sessions` / `auth:revoke <email>` | sign-in setup, groups and sessions | — |
| `berth storage:mount\|unmount\|list <app> <path>` | persistent folders | `storage:*` |
| `berth pg:create\|link\|unlink\|psql\|backup\|restore\|expose\|upgrade\|destroy\|list` | Postgres service | dokku-postgres |
| `berth redis:*` | Redis service (same verbs; `redis:cli` instead of `pg:psql`) | dokku-redis |
| `berth releases <app>` | list releases with sha, status, time | `ps:report` |
| `berth run <app> <cmd>` | one-off command in a fresh container with the app's env (migrations) | `run` |
| `berth exec <app> <cmd>` | shell into the running container | `enter` |
| `berth dockerfile <app>` | print the template Berth would use | — |
| `berth git:remote <app> [--host h]` | add a `berth` SSH remote for push-to-deploy | built in |
| `berth app:set-key <app>` | fetch over SSH with a generated deploy key | `git:*` |
| `berth doctor` / `berth events` / `berth gc` / `berth backup` / `berth restore` | ops | — |
| `berth reboot` | planned reboot that comes back unattended (`fdesetup authrestart`) | — |
| `berth reload` | re-apply `~/.berth` to containers, Caddy and the tunnel after hand edits | — |
| `berth cf:login <zone>` | connect another Cloudflare zone by browser login, for `domains:add` | — |
| `berth trust-ca` | trust Caddy's local CA in the login keychain | — |
| `berth uninstall [--purge]` | remove the launchd agents and stop everything (`--purge` also deletes apps, volumes, images and `~/.berth`) | — |
| `berth notify:set <url\|off>` | deploy/crash notifications | — |

Output conventions: success ends with the URL or the next command; errors are one line plus `berth doctor` or `berth logs` as the pointer; long operations stream (`building ... ✓ 23 s`). Shell completions (`berth completion zsh|bash|fish`) ship with the brew formula.

## Implementation plan for a coding agent

Go 1.23+, one module, two binaries from one `cmd/`. Shell out to `container`, `git`, `cloudflared`; talk to Caddy over HTTP. No CGO, no database, no framework beyond `cobra` for the CLI (other dependencies: `golang.org/x/crypto/bcrypt` for basic auth, `golang.org/x/term`). Target was \~6k lines including tests; v1 came to about 10.5k plus 1.1k of tests. Each milestone ends with a demo the agent can run end-to-end on a real Mac mini; do them in order and do not start the next until the demo passes.

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
    daemon/     # RPC handlers, webhook server, supervisor, log tails, poller, scheduler, status, backup/restore, mDNS, Tailscale
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

**Milestone status (v1)**: 1, 2, 3, 6 and 8 are done and demoed on a real Mac (the Postgres demo used a Flask app rather than Prisma; nightly dumps are implemented but were not observed firing at 03:00). 5 is done except against real GitHub: HMAC, redelivery, branch filtering, polling and push-to-live were demoed with signed local requests and a local repo; webhook registration and the API calls are tested against mock servers. 7's supervisor half is done (crash restart in 8 s, berthd self-restart on upgrade); the launchd half is untested. 4 is implemented but untested without a Cloudflare account: `cloudflared tunnel ingress validate` accepts the generated config and routes each hostname as designed, and the API calls are tested against a mock. The README GIF is not made.

**Acceptance tests** (checked, not aspirational): the three-command install on a fresh macOS 26 account completes in < 5 min with no `sudo` beyond the two consented commands; push-to-live p50 < 45 s for a Node app with a warm cache; zero failed requests across 20 consecutive deploys under load; power-cycle recovery < 2 min with all apps and Postgres healthy (in `--unattended` mode, or measured from the FileVault unlock otherwise); `berth restore` on a second Mac reproduces every app and database; `go test ./...` runs on Linux with the fake runtime and covers state, deploy ordering, HMAC, detection and Caddy config generation.

Results so far: zero failed requests across 20 deploys and a restart through the real Cloudflare tunnel at 20 requests/s (1,325 requests, p50 28 ms), and locally across 20 deploys at 100 requests/s (5,071 requests); push-to-live through a real GitHub webhook about 8 s for the hello-world FastAPI app, and 12 s with `git push berth` over SSH; crash-loop give-up after 10 crashes with the notification delivered through ntfy.sh; push-to-live about 3 s for a small Node app with a warm cache; `berth restore` after a full purge reproduced every app, database, Redis key, volume and config var in 15 s on the same Mac; `go test -race ./...` passes and every package compiles for Linux. The install, power-cycle and second-Mac tests have not been run. Google sign-in worked end to end on the first real install: `auth:setup google`, then sign-in on `auth.cronbon.com` and a one-redirect hand-off to Chirp (`berth access chirp public`), whose posts now carry the Google identity.

**Guidance for the agent**: read Apple's `container` docs (`github.com/apple/container/docs`) for the exact flags before writing `runtime/`; prefer one shell-out with `--format json` over parsing text; make every write to `~/.berth` atomic (write temp, rename); log every external command at debug level with its duration; keep each CLI command under 150 lines; when in doubt, copy the Dokku behaviour and name.

## Open questions and known limitations

Things to settle in milestone 1 by trying them, and limits worth stating in the README. Answers from building v1 on macOS 26.1 with `container` 1.6.1 are recorded inline.

- [x] **Volume semantics in `container`**: a named volume is an ext4 disk image in `~/Library/Application Support/com.apple.container/volumes/` and cannot be pinned elsewhere. Bind-mounting a host folder works for app storage but not for Postgres (its entrypoint's `chown` fails on virtiofs). Postgres uses named volumes; Time Machine sees the nightly dumps, not the live data.
- [x] **Build cache**: yes. The BuildKit builder VM keeps its layer cache across builds (a one-line change rebuilt in 0.6 s). Measured push-to-live with a warm cache: about 3 s for a small Node app.
- [x] **DNS domain per network**: no. Container-to-container name resolution needs `sudo container system dns create <domain>`. Berth publishes services on the network gateway instead (see Networking), which keeps setup free of `sudo`.
- [x] **Idle memory**: measured by `container stats`: about 35 MB for a minimal Python server, 60 MB for Caddy serving a static site, 105 MB for gunicorn + Flask, 130 MB for Node 22, 36 MB for Redis and 210 MB for Postgres 18. Node misses the 60 MB target; the 20-apps-on-16-GB estimate still holds with room to spare.
- [ ] **launchd + `container system`**: confirm the service starts cleanly at login with no GUI session interaction, and confirm that `fdesetup authrestart` brings the user session (and so the LaunchAgents) back without a login prompt on macOS 26. *Not yet verified: the generated plists pass `plutil -lint`, but loading them was outside what the v1 smoke test was allowed to do.*
- [x] **Container network loss**: twice in two hours of testing, every container lost outbound internet (`Network is unreachable`, builds stuck on `pip install`, apps unable to call external APIs) while the Mac itself was fine and inbound traffic still worked. `container system stop && container system start` fixes it, and costs about 6 s of downtime while berthd brings the services and apps back. berthd now probes outbound access every 5 minutes from a throwaway Alpine container (every minute once a probe fails). After two failures in a row, with no deploy in flight, it restarts the container system itself and sends a `network` notification; this doesn't count as a crash. Set `auto_heal_network: false` in `config.json` to get only the notification. Builds that fail with network-error signatures say so, and `berth doctor` runs the same probe.
- [ ] **Rosetta**: should `berth` pass `--rosetta`-style flags for amd64-only images automatically, or just document "use arm64 images"? Default: detect `linux/amd64`-only manifests and warn. *Not implemented in v1; the builder VM has Rosetta enabled by default (`container system property list`), so amd64 base images build.*
- [x] **Name**: *Berth*, at github.com/dps/berth.
- [ ] **GitHub device flow**: needs Berth's GitHub OAuth app (device flow enabled; its public client ID is compiled in as `github.DefaultClientID`, overridable with `BERTH_GITHUB_CLIENT_ID`).
- [x] **Cloudflare login-cert token permissions**: verified on cronbon.com: the token in `cloudflared tunnel login`'s cert can read the zone and its DNS records, delete records, and read and write the tunnel's remote configuration. DNS records are created with `cloudflared tunnel route dns`.
- [ ] **Tailscale mode**: implemented with Tailscale Services (`tailscale serve --service=svc:<app>`, one MagicDNS name per app, approved once in the admin console) but not yet tested against a tailnet.

**Known limitations, v1**

- With FileVault on, an unplanned power loss needs a person to unlock the Mac before apps come back; a UPS or `--unattended` mode avoids it.
- Apple Silicon + macOS 26 only; no Intel, no Linux. (The abstraction in `runtime/` would admit a Docker backend later, but that is not a goal.)
- One process type per app; no cron scheduler; no multi-host.
- Cloudflare account required for public URLs (free tier is fine). Tailscale/LAN modes exist for people who do not want Cloudflare.
- A home connection's upload speed is the ceiling; Berth is for apps, not for serving large media.
- Postgres and Redis only; other services (Mongo, MySQL, MinIO) are either a Dockerfile in your own app or a v2 plugin API, deliberately deferred until two people ask.

**Sources**: [apple/container](https://github.com/apple/container) · [container CLI `run` reference](https://mintlify.com/apple/container/cli/run) · [container networking notes](https://instagit.com/apple/container/how-to-network-multiple-containers/) · [Dokku docs](https://dokku.com/docs/) · [Cloudflare Tunnel docs](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) · [Caddy admin API](https://caddyserver.com/docs/api)
