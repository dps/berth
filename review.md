# Berth security + bug review

Oct 25, 2026 · reviewer: Muse Code (model) · scope: `design.md` + full codebase (~12.6k lines Go + templates)

## Verdict

Solid posture for a single-user system. Webhook HMAC verification is
constant-time, basic-auth passwords are bcrypt-hashed, all writes are atomic,
secrets are 0600, nearly every subprocess call is argv-only (no shell), SQL
identifiers and literals are quoted on top of a validated name charset, and
`go vet ./...` is clean. No remote unauthenticated code execution was found.

One genuine shell injection exists (`--branch` → post-receive hook), plus
several medium issues and a list of low-severity hardening nits, below.
Findings are ordered by severity; each names the file, the impact, and the fix.

---

## High

### H-1. Shell injection via unvalidated `--branch` in the `git push berth` hook

- **File:** [internal/gitx/gitx.go](/Users/geist/proj/berth/internal/gitx/gitx.go:179)
  (`InstallPostReceive`)
- **What:** the branch is interpolated into a shell script without escaping:

  ```sh
  if [ "$ref" = "refs/heads/%[3]s" ] && ...
  ```

  `--branch` is never validated (neither CLI nor daemon), so
  `berth create x --branch 'x" ]; evil; #'` writes a hook that runs `evil`
  on the next push. The other fields use Go `%q`, which is also not
  shell-safe: `$` and backticks survive verbatim inside shell double quotes.
- **Impact:** bounded — the attacker needs `berth create` access (same user,
  or an agent acting on untrusted input). Still the only true command
  injection in the repo.
- **Fix:** validate branch against git ref rules at create time, and
  single-quote-escape (`'\''`) every interpolated field in the hook template.

## Medium

### M-1. Git option injection via `--repo` / `--ref` starting with `-`

- **File:** [internal/gitx/gitx.go](/Users/geist/proj/berth/internal/gitx/gitx.go:91)
  (`Fetch`, `FetchSHA`, `LsRemote`, `Resolve`); no `--` separator anywhere.
- **What:** a repo such as `--upload-pack=<cmd>` is parsed by git as an
  option, and `ls-remote`/`fetch --upload-pack` executes a local command.
- **Impact:** same-user/agent vector, same as H-1.
- **Fix:** reject repo/branch/ref values starting with `-` (or insert `--`
  where git accepts it).

### M-2. `redis:unlink` does not revoke access

- **File:** [internal/services/services.go](/Users/geist/proj/berth/internal/services/services.go:354)
  (`Unlink`)
- **What:** Redis links share the service password; unlink only deletes the
  bookkeeping record. A stale `REDIS_URL` keeps working indefinitely —
  unlike `pg:unlink`, which revokes the role's login.
- **Fix:** rotate the service password on unlink and update the remaining
  links (document if intentionally deferred).

### M-3. Webhook rate limit is consumed before authentication

- **File:** [internal/daemon/webhook.go](/Users/geist/proj/berth/internal/daemon/webhook.go:55)
  (`allowHook` runs before the HMAC check at line 73)
- **What:** anyone on the internet can burn an app's 30/min budget with
  bad-signature requests, delaying legitimate deliveries (branch polling
  bounds this to ~60 s). Separately, the 404-vs-401 response split lets
  anyone enumerate valid app names.
- **Fix:** rate-limit signature failures by source IP separately from the
  per-app deploy budget, and return a uniform status for unknown apps vs.
  bad signatures.

### M-4. Passwords leak into the process list, contradicting the design doc

- **Files:**
  - `psql -c "<SQL containing 'password'>"` —
    [internal/services/services.go](/Users/geist/proj/berth/internal/services/services.go:272)
  - multi-line app env passed via `-e` —
    [internal/deploy/deploy.go](/Users/geist/proj/berth/internal/deploy/deploy.go:527)
    (see `ContainerEnvFile` in
    [internal/state/env.go](/Users/geist/proj/berth/internal/state/env.go:113))
- **What:** secrets travel in argv, visible to other local users via `ps`.
  `redactArgs` only scrubs debug logs, not the process table. The design doc
  promises "service passwords travel by `--env-file`, not argv".
- **Fix:** feed SQL via stdin or a 0600 temp file; pass multiline values
  through a second env file if the runtime allows it.

### M-5. WAF filter expression built by string interpolation; `--domain` never validated

- **Files:** [internal/tunnel/tunnel.go](/Users/geist/proj/berth/internal/tunnel/tunnel.go:403)
  (`FilterExpression`); domain accepted raw at
  [internal/cli/init.go](/Users/geist/proj/berth/internal/cli/init.go:94)
- **What:** `cfg.Domain` and unvalidated GitHub `/meta` CIDRs are spliced
  into a Cloudflare expression with `%s` and no escaping. A quote in either
  breaks or injects into the rule (failing open to HMAC-only, but silently).
- **Fix:** validate `--domain` as a hostname at init; validate each CIDR
  with `netip.ParsePrefix` before interpolating.

### M-6. `validHost` allows `*`; no collision check against built-in hosts

- **File:** [internal/daemon/handlers.go](/Users/geist/proj/berth/internal/daemon/handlers.go:548)
  (`validHost`, `hDomainsAdd`)
- **What:** `domains:add evil '*.example.com'` is accepted, and Caddy
  matches routes first-wins in app-name order, so one app can shadow
  another's traffic (or `hooks.*`). Nothing checks collisions with
  `<app>.<domain>` / `<app>.<host>.local` either.
- **Impact:** self-inflicted on a single-user box, but tightening is cheap.
- **Fix:** reject wildcards, enforce hostname charset/length, and check
  collisions with built-in hostnames.

## Low / hardening

### L-1. `config.json` is 0644 and contains `NotifyURL` (usually a bearer secret)

- **File:** [internal/state/state.go](/Users/geist/proj/berth/internal/state/state.go:149)
  (`SaveConfig`). Same concern for app data dirs created 0755
  ([handlers.go](/Users/geist/proj/berth/internal/daemon/handlers.go:777),
  [deploy.go](/Users/geist/proj/berth/internal/deploy/deploy.go:521)) —
  SQLite DBs and uploads readable by other local users.
- **Fix:** 0600 for `config.json`, 0700 for data dirs.

### L-2. Unbounded build log + unbounded RPC stream line buffer

- **Files:** build log write in
  [internal/deploy/deploy.go](/Users/geist/proj/berth/internal/deploy/deploy.go:337)
  (every other log rotates; this one doesn't); `Stream.buf` grows without
  limit until a newline in
  [internal/rpc/rpc.go](/Users/geist/proj/berth/internal/rpc/rpc.go:44)
- **What:** hostile or merely verbose build output can fill the disk, and a
  single line without `\n` can OOM the daemon.
- **Fix:** cap line length in `Stream.Write` and rotate/cap build logs.

### L-3. `rand.Read` errors ignored for secrets

- **Files:** [internal/daemon/handlers.go](/Users/geist/proj/berth/internal/daemon/handlers.go:100)
  (`randomHex`, webhook secrets),
  [internal/services/services.go](/Users/geist/proj/berth/internal/services/services.go:68)
  (`Password`, DB passwords)
- **What:** a (practically impossible) `crypto/rand` failure yields
  predictable all-zero secrets.
- **Fix:** check the error and panic.

### L-4. `svc.link --as` env var name never validated

- **File:** [internal/services/services.go](/Users/geist/proj/berth/internal/services/services.go:306)
  (`Link`; called from `hSvcLink`)
- **What:** a newline or `=` in the name corrupts the env file and injects
  extra vars into the container env. (`config:set` validates; this path
  doesn't.)
- **Fix:** run it through `ValidateEnvKey`.

### L-5. TMPDIR socket fallback: predictable path, no ownership check

- **Files:** [internal/state/state.go](/Users/geist/proj/berth/internal/state/state.go:44)
  (`SocketPath`); listen + deferred chmod in
  [internal/daemon/daemon.go](/Users/geist/proj/berth/internal/daemon/daemon.go:282)
- **What:** with home paths ≥100 chars the socket lands at a predictable
  `$TMPDIR/berth-<hash>.sock`; the CLI connects to whatever is there, and
  the socket is briefly world-accessible before `chmod 0600`. A pre-planted
  socket could harvest `config:set` secrets from the CLI.
- **Fix:** use a 0700 subdir and/or verify socket ownership before listen
  and connect.

### L-6. `Apply` trusts `a.Name` from file content instead of the directory name

- **File:** [internal/daemon/daemon.go](/Users/geist/proj/berth/internal/daemon/daemon.go:162)
- **What:** since hand-edit + `reload` is a supported path, a mismatched
  `name` field in `app.json` can redirect the Caddy access-log path and
  generated hostnames.
- **Fix:** treat the directory name as canonical.

### L-7. `hDomainsAdd` touches DNS before checking the app exists

- **File:** [internal/daemon/handlers.go](/Users/geist/proj/berth/internal/daemon/handlers.go:555)
- **What:** a typo'd app name orphans a CNAME record.
- **Fix:** verify the app first, then add DNS.

### L-8. `berth run` temp env file lingers on SIGKILL

- **File:** [internal/cli/interactive.go](/Users/geist/proj/berth/internal/cli/interactive.go:52)
- **What:** the 0600 file holds live secrets and is only removed on the
  graceful path, so kills accumulate secret-bearing `.run-env-*` files.
- **Fix:** sweep stale ones on start (or use a trap + a dedicated tmp dir).

### L-9. Restore honors archive file modes

- **File:** [internal/daemon/backup.go](/Users/geist/proj/berth/internal/daemon/backup.go:179)
  (`extractArchive`; the `..`/absolute-path guard itself is good)
- **What:** a crafted archive restores secrets world-readable. (Round-trips
  of Berth's own 0600 backups are fine.)
- **Fix:** force 0600/0700 on extract.

### L-10. `config:show` masking gaps

- **File:** [internal/state/env.go](/Users/geist/proj/berth/internal/state/env.go:134)
  (`IsSecretKey`)
- **What:** bare `TOKEN` / `SECRET` / `AUTH`-style names (no `_KEY` /
  `_SECRET` / `_TOKEN` suffix, not `PASSWORD`) print unmasked. Cosmetic —
  `--reveal` exists — but easy to extend.

## Correctness bugs (non-security)

### B-1. Env values with leading/trailing spaces don't round-trip

- **File:** [internal/state/env.go](/Users/geist/proj/berth/internal/state/env.go:98)
  (`Format` vs `ParseEnv`)
- **What:** `Format` only quotes values containing `\n`, `\r`, `"` or a
  leading `'`, but `ParseEnv` trims spaces — so `" secret "` silently
  becomes `"secret"` on the next read.

## Audited and found sound (no action)

- Webhook HMAC-SHA256 + `hmac.Equal`, 1 MB body cap, delivery-id dedupe,
  branch/delete/path filtering.
- `bcrypt.DefaultCost` for basic auth; >72-byte passwords fail closed.
- All subprocess invocations are argv-only except the H-1 hook template
  (tailscale, dns-sd, ssh-keygen, git, cloudflared, doctor checks all pass
  user-influenced values as argv).
- Postgres `quoteIdent`/`quoteLit` used consistently, on top of a
  DNS-label-validated charset for service/app names.
- Caddy config and tunnel ingress built via JSON marshal / `%q` YAML —
  no injection through hostnames except the M-5/M-6 expression/matcher
  semantics.
- `detect/` templates: repo-controlled strings flow only into
  Dockerfile/CMD content executed inside the container or builder VM —
  no host impact.
- Launchd plist pre-escapes XML correctly.
- RPC `status`/`releases`/`svc:list` responses contain no secrets; the one
  intentional disclosure (`pg:expose` printing the password) is the
  feature, not a leak.
- Unauthenticated RPC socket (0600) and Caddy admin on loopback are
  appropriate: the daemon shares a UID with every local caller, so no
  privilege boundary is crossed, and containers cannot reach host loopback.
- Tokenless branch polling will hit GitHub's 60 req/hr unauthenticated
  rate limit past ~1 app — availability note, not a vulnerability.
- `cloudflared --no-autoupdate` pins the tunnel binary to Homebrew
  updates; worth a conscious decision either way.

---

## Disposition (Oct 25, 2026)

Each finding was checked against the code and, where it mattered, against
the running system before deciding. Fixed in the commit after `521a45b`.

| Finding | Decision |
| --- | --- |
| H-1 shell injection via `--branch` | **Fixed.** Branches, paths and refs are validated (git ref rules, no leading `-`) in the RPC handlers, and the hook single-quotes every field. A test runs the generated hook with hostile app, branch, path and binary names and checks nothing executes; it fails against the old hook. |
| M-1 git option injection | **Fixed.** Repos can't start with `-` or contain shell metacharacters; `ls-remote` and `Resolve` refuse option-like arguments. |
| M-2 `redis:unlink` didn't revoke | **Fixed differently.** Rotating the shared password would redeploy every other linked app; instead each link gets its own Redis ACL user (persisted in `/data/users.acl`), and unlink runs `ACL DELUSER`. Verified on the real install: the app's credentials return `OK PONG` while linked and `WRONGPASS` after unlink. Existing services migrate on their next link. |
| M-3 rate limit before auth; 404 vs 401 | **Fixed.** Bad signatures are limited per source IP before any work; only authenticated deliveries use the app's budget; unknown apps return 401 like a bad signature. Test: 50 junk requests from one IP don't block a signed delivery. |
| M-4 passwords in argv | **Fixed for SQL** (now over stdin to `psql`; Redis ACL commands likewise). **Not fixed for multi-line env values**: `container run --env-file` can't carry newlines, so `-e` is the only way; the design doc now states this exception instead of claiming there is none. |
| M-5 WAF expression interpolation | **Fixed.** `--domain` is validated as a hostname; CIDRs from `/meta` go through `netip.ParsePrefix`; a host with quotes is refused. |
| M-6 wildcard / colliding custom domains | **Fixed.** Hostnames must be plain DNS names; collisions with `hooks.<domain>`, other apps' `<app>.<domain>` and `.local`/tailnet names are refused. |
| L-1 `config.json` 0644 | **Fixed** (0600). The underlying risk is small: `~/.berth` is `drwx------`, so nothing under it was readable by other users. Data dirs left as they are for the same reason (and tightening them risks breaking bind mounts). |
| L-2 unbounded build log / RPC line buffer | **Fixed.** Build logs stop at 50 MB with a note; RPC lines over 64 KB are sent in pieces. |
| L-3 `rand.Read` errors ignored | **No change.** Since Go 1.24, `crypto/rand.Read` never returns an error (it crashes the program), so there's no all-zero-secret path. |
| L-4 `--as` unvalidated | **Fixed** (`ValidateEnvKey`). |
| L-5 predictable TMPDIR socket | **No change.** On macOS `$TMPDIR` is a per-user `drwx------` directory, so the path isn't reachable by other users, and the fallback only applies when `BERTH_HOME` is extremely long. |
| L-6 trusting `name` inside `app.json` | **Fixed.** The directory name is authoritative. |
| L-7 DNS before app check | **Fixed.** `domains:add` checks the app exists first. |
| L-8 stale `berth run` env files | **Fixed.** Files older than an hour are swept on the next `berth run`. |
| L-9 restore honours archive modes | **Fixed.** Extracted files are 0600, directories 0700. |
| L-10 masking gaps | **Fixed.** Masks any `_`-separated word like `TOKEN`, `SECRET`, `AUTH`, `PASS`, `DSN`…, plus any value that is a URL with a password or a PEM block. |
| B-1 whitespace round-trip | **Fixed.** Values with leading/trailing whitespace are quoted. |
| Note: unauthenticated polling hits GitHub's 60/hr limit | **Fixed.** Without a token, GitHub repos are polled with `git ls-remote`. |
| Note: `cloudflared --no-autoupdate` | **Kept**, deliberately: Homebrew owns cloudflared's updates, and self-updating binaries would fight it. |
