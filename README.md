# Berth

A Dokku for Mac minis. `git push` to GitHub and your app is live at
`https://app.yourdomain.com` in under a minute, with Postgres one command away.

Berth composes four things that are each very good at one job — Apple's
[`container`](https://github.com/apple/container) (one micro-VM per app),
[Caddy](https://caddyserver.com) (routing), [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
(public ingress with no open ports) and GitHub webhooks (deploy trigger) —
behind a Dokku-shaped CLI. Everything lives in `~/.berth`; no `sudo` after
install, no YAML, no Docker Desktop.

Requires an Apple Silicon Mac on macOS 26 or later.

```bash
brew tap dps/berth https://github.com/dps/berth
brew install --HEAD berth                # until a release is tagged
berth init --domain example.com          # once, ~3 minutes
berth create myapp --repo me/myapp       # per app
git push                                 # every deploy
```

## The daily loop

```bash
git push                                 # webhook → build → zero-downtime swap
berth logs myapp -f
berth config:set myapp OPENAI_API_KEY=sk-...
berth rollback myapp                     # previous release back in ~2 s
berth status                             # every app, release, uptime, URL, disk
```

Need a database?

```bash
berth pg:create mydb
berth pg:link mydb myapp                 # sets DATABASE_URL, redeploys
berth storage:mount myapp /data          # persistent folder (SQLite, uploads)
```

Your app must listen on `$PORT` on `0.0.0.0`, read config from env, write
only to `/tmp` or a mounted volume, and log to stdout — the Heroku/Dokku
contract. Repos without a Dockerfile get one generated for Node, Bun,
Python, Go, Rust and static sites; `berth dockerfile myapp` prints it.

Run `berth --help` for every command, and `berth doctor` whenever something
looks wrong: it checks each assumption Berth makes and prints a fix.

## How it works

```
 GitHub ──webhook──▶ Cloudflare ──tunnel──▶ cloudflared ─┬─▶ berthd :7777 (hooks.<domain>)
 visitor ───────────▶   edge                             └─▶ Caddy 127.0.0.1:7780 (*.<domain>)
                                                                 │
 berth CLI ──unix socket──▶ berthd ──container build/run──▶ app VMs (127.0.0.1:8001-8999)
                                   └─ Caddy admin API             │ berth network
                                                                  ▼
                                                  pg-<name> / redis-<name> VMs
```

- **berthd** (a launchd agent) receives webhooks, builds with
  `container build`, starts the new release next to the old one, waits for
  it to answer, points Caddy at it, then stops the old one. It also restarts
  crashed apps, tails logs into `~/.berth/apps/<app>/logs`, polls branches
  as a webhook safety net, and dumps Postgres nightly.
- **Caddy** routes `<app>.<domain>` (from the tunnel) and
  `<app>.<mac>.local` (LAN, HTTPS with Caddy's internal CA, advertised over
  Bonjour).
- **State** is plain files under `~/.berth` — `cat` it, `tar` it,
  hand-edit it and run `berth reload`.

The full design, including what was learned building it, is in
[design.md](design.md).

## Reboots and FileVault

With FileVault on (the default), crashes and restarts recover by
themselves and `berth reboot` does planned reboots unattended
(`fdesetup authrestart`), but after a power cut someone has to type the
password at the Mac. Use a small UPS, or `berth init --unattended` with
FileVault off and automatic login. `berth doctor` reports which mode you are in.

## Development

```bash
make build      # bin/berth and bin/berthd
make test       # go test -race ./... (runs on Linux too, with a fake runtime)
```

To try Berth without touching `~/.berth` or the default ports, point
`BERTH_HOME` at a scratch directory, run `berth init --no-tunnel --no-launchd`,
edit the ports in `$BERTH_HOME/config.json`, and start `caddy run --config
$BERTH_HOME/caddy.json` and `berthd` yourself.

## License

MIT
