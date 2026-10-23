# Tofu gallery copy

This repository is Tofu's copy of **sysadminsmedia/homebox** (licence: AGPL-3.0), listed in Tofu's app gallery as **Homebox**: a home inventory app for recording what you own, where it is kept, its receipts, warranties and maintenance.

- `main` mirrors the upstream repository and is never committed to by Tofu.
- `tofu` carries Tofu's own changes; tags named `pin/<date>` mark the versions the gallery points to. The current pin is `pin/2026-10-23.1`.
- A visitor who chooses this app gets a private snapshot of the pinned version in their own Tofu account. Their copy does not follow upstream updates.
- Upstream: https://github.com/sysadminsmedia/homebox at `1fd28822634a3d5935c002d569559ef46530d4a0` (2026-10-17, v0.27.0 plus one commit).

## How Tofu runs it

- Framework preset `container`, root folder: the repository root. Tofu builds the root `Dockerfile` and runs the image as a service, so there is no separate install command, build command or output directory.
- The `Dockerfile` is a multi-stage build: the frontend with Node 22 and pnpm 10 (`pnpm install --frozen-lockfile`, then `pnpm build`, which runs `nuxt generate`), embedded into the Go server (`go build ./app/api/*.go`), on Alpine. The Go step uses a BuildKit cache mount (`RUN --mount=type=cache`).
- Database: PostgreSQL, the app's Tofu database, read from `DATABASE_URL`. Without `DATABASE_URL`, Homebox falls back to SQLite under `/data`, which is lost on every restart.
- Environment: Tofu sets `PORT` and `DATABASE_URL`. The image sets `HBOX_THUMBNAIL_ENABLED=false`, `HBOX_OPTIONS_GITHUB_RELEASE_CHECK=false` and `HBOX_OPTIONS_SOURCE_URL`. Nothing has to be set by hand before the first deploy. Settings an owner may add: `HBOX_AUTH_API_KEY_PEPPER` (see below), `HBOX_OPTIONS_ALLOW_REGISTRATION=false`, and `HBOX_STORAGE_CONN_STRING` with a bucket's keys.

## What Tofu changes

- `tofu-entrypoint.sh` (new). The image now starts through this script. It does three things, then runs `/app/api` with the same arguments:
  - It sets `HBOX_WEB_PORT` to Tofu's `PORT`. Tofu sends traffic to `PORT`, and Homebox would otherwise listen on 7745. Without `PORT`, the upstream default of 7745 stays.
  - When `DATABASE_URL` is set, it splits it into the fields Homebox reads: `HBOX_DATABASE_DRIVER=postgres`, `HBOX_DATABASE_HOST`, `HBOX_DATABASE_PORT`, `HBOX_DATABASE_USERNAME`, `HBOX_DATABASE_PASSWORD`, `HBOX_DATABASE_DATABASE` and `HBOX_DATABASE_SSL_MODE`. Homebox has no setting that takes a URL. Any `HBOX_DATABASE_*` value the owner sets is kept.
  - When `HBOX_AUTH_API_KEY_PEPPER` is not set and `DATABASE_URL` is, it sets the pepper to the SHA-256 of `homebox-api-key-pepper:` followed by the whole `DATABASE_URL`, written as 64 hex characters. Homebox refuses to start without a pepper of at least 32 bytes, and Tofu does not supply one, so this lets a copy start with nothing set by hand. A pepper the owner sets always wins. See "The API key pepper" below.
- `Dockerfile`:
  - `ENTRYPOINT` is now `/bin/sh /app/tofu-entrypoint.sh`, and the script is copied into the image. The `CMD` is unchanged.
  - `HBOX_THUMBNAIL_ENABLED=false` and `HBOX_OPTIONS_GITHUB_RELEASE_CHECK=false` are now image defaults, and the owner can set either back to `true`. Thumbnails cost image processing for photos that are lost on restart anyway (see below). The release check calls GitHub every hour to show an update notice, but this copy is a fixed snapshot.
  - `HBOX_OPTIONS_SOURCE_URL=https://github.com/tofu-gallery/homebox/tree/pin/2026-10-23.1` is an image default: the address of this copy's exact source (see "Licence and source code").
  - `HEALTHCHECK` checks the port the server actually listens on (`PORT`, then `HBOX_WEB_PORT`, then 7745), not a fixed 7745.
- `backend/app/api/setup.go`: when `DATABASE_URL` is set, the Postgres connection adds `default_query_exec_mode=cache_describe`. Tofu's `DATABASE_URL` goes through a transaction pooler, which keeps no named prepared statements, and those are pgx's default. With this mode, every query runs on the unnamed statement.
- A source link the copy can point at its own code, for AGPL-3.0 section 13:
  - `backend/internal/sys/config/conf.go` adds the `HBOX_OPTIONS_SOURCE_URL` setting (`options.source_url` in a config file), empty by default.
  - `backend/app/api/handlers/v1/controller.go` returns it as `sourceUrl` from `/api/v1/status`, and leaves the field out when it is empty.
  - `frontend/pages/index.vue`: the GitHub icon on the sign-in page opens that address instead of upstream's repository.
  - `frontend/layouts/default.vue`: the footer of every signed-in page shows a "GitHub Project" link to that address instead of upstream's "Version: … Build: …" release link.
  - `frontend/lib/api/types/data-contracts.ts` declares the field. That file and the API docs under `backend/app/api/static/docs` are generated upstream; the docs were not regenerated, so they do not list `sourceUrl`.
  - With `HBOX_OPTIONS_SOURCE_URL` empty, every link is exactly as upstream has it.

## The API key pepper

Homebox stores each API key as an HMAC keyed with `HBOX_AUTH_API_KEY_PEPPER`. Sign-in sessions use a plain hash without the pepper.

- On Tofu the pepper is derived from `DATABASE_URL` each time the container starts (above), so nothing has to be set.
- Anything that changes `DATABASE_URL` changes the pepper at the next start: a new database password, a different host, port or pooler address, other query options, or a database Tofu brings back or creates anew under a different address. After that start, every API key issued before stops working: a request with one gets `401 valid authorization token is required`. The keys still appear on the profile page and have to be deleted and created again. Sign-in sessions, passwords and the inventory itself are not affected.
- To keep API keys independent of the database address, set `HBOX_AUTH_API_KEY_PEPPER` as a hidden value on the app's Keys & settings page, at least 32 bytes, for example the output of `openssl rand -base64 48`, and keep it unchanged afterwards. Set it before creating any API key: moving from the derived pepper to your own also cancels the keys issued until then.
- The derived pepper is only as secret as `DATABASE_URL`, which Tofu keeps as a hidden value.
- Without `DATABASE_URL` (no Tofu database) and without `HBOX_AUTH_API_KEY_PEPPER`, Homebox stops at start with `auth.api_key_pepper must be set to at least 32 bytes`.

## What works differently on Tofu

- **Item photos and attachments need a storage driver.** They are written to `/data` inside the container (`HBOX_STORAGE_CONN_STRING=file:///?no_tmp_dir=true`), and that folder is lost on every restart and deploy. To keep them, point `HBOX_STORAGE_CONN_STRING` at a bucket and give the app that bucket's keys as hidden values. The image already includes the `s3://`, `gs://` and `azblob://` drivers.
- **Live refresh over a websocket is off.** A page does not update by itself when something changes in another tab or on another device; reload it to see the change. The browser console may show the page retrying `/api/v1/ws/events`.
- **Maintenance reminders need a trigger.** The running server checks once an hour. During the 08:00 hour (UTC in this image), it sends the day's scheduled maintenance through the notifiers the owner set up. If the app is asleep or restarting at that hour, nothing is sent that day. Reliable reminders need something that wakes the app on a schedule.
- **Homebox has its own login, not Tofu sign-in.** People create a Homebox account on the app's own page. Anyone who can open the address can register until the owner sets `HBOX_OPTIONS_ALLOW_REGISTRATION=false` after creating their own account, or makes the app private on Tofu.
- **The version and build numbers are blank.** The `Dockerfile` takes `VERSION`, `COMMIT` and `BUILD_TIME` as build arguments. A build given none, as in the local check below, reports all three empty, so the sign-in page's footer reads `Version: ~ Build:`.

## Needs a real deploy to verify

- The image builds on Tofu's container hosting, including the BuildKit cache mount.
- The server answers `/` and `/api/v1/status` on `PORT`, and the log says `Server is running on :<PORT>`.
- The first start connects through Tofu's `DATABASE_URL` and runs all the goose migrations against the pooler.
- Sign-up, sign-in, and creating, editing, searching and deleting items all work. The runtime logs show no `prepared statement` errors (SQLSTATE 26000 or 42P05).
- An uploaded item photo shows until the next restart, which shows `/data` is writable in Tofu's container.
- Whether `/api/v1/ws/events` upgrades to a websocket on Tofu at all.
- Whether Tofu's build passes `VERSION` or `COMMIT` build arguments (the version shows blank if not).
- Whether sign-in rate limiting sees each visitor's own address. Behind Tofu's proxy, every visitor may share one address. Then five failed sign-ins from anyone would slow sign-in for everyone, and five sign-ups a minute would be the limit for the whole app. `HBOX_OPTIONS_TRUST_PROXY=true` reads the forwarded address instead, but only set it if Tofu's proxy sets that address and visitors cannot forge it.

## Licence and source code

- AGPL-3.0. `LICENSE` and every copyright notice stay exactly as upstream has them. Every file Tofu changes carries a dated `Tofu (2026-10-23)` note at the change, and `tofu-entrypoint.sh` is Tofu's own file.
- The Homebox name and logo belong to the upstream project; the gallery uses them only to say which app this is. `backend/internal/core/currencies/currencies.json` is upstream's bundled currency list. No fonts are bundled.
- **Source offer (AGPL-3.0 section 13).** A modified copy that people use over a network has to offer them its own source code.
  - What a copy of the earlier pin, `pin/2026-10-23`, shows: the sign-in page's GitHub icon opens upstream's repository, https://github.com/sysadminsmedia/homebox, and the signed-in footer links "Version: … Build: …" to an upstream release page, which for a build without version arguments is `https://github.com/sysadminsmedia/homebox/releases/tag/v` and does not exist. Neither reaches Tofu's changes.
  - From `pin/2026-10-23.1`, the image sets `HBOX_OPTIONS_SOURCE_URL` to this repository at the exact tag, https://github.com/tofu-gallery/homebox/tree/pin/2026-10-23.1, and both the sign-in page's GitHub icon and the signed-in footer's "GitHub Project" link open it. This repository is public, and the tag never moves.
  - An owner who changes their copy's code runs a modified version of their own. They have to publish that source and set `HBOX_OPTIONS_SOURCE_URL` to it on the app's Keys & settings page. The server reads it at start, so it takes effect with the next deploy and needs no code change.

## Verification

2026-10-23, on macOS arm64 against 127.0.0.1 only, from the `tofu` branch with this pin's changes. No Docker, nothing deployed.

- Frontend, with pnpm 10.28.0 (the `packageManager` version): `pnpm install --frozen-lockfile` finished in 16.9 s; `pnpm build` (`nuxt generate`) printed `Generated public .output/public`. `pnpm typecheck` fails on upstream's code as well: 192 errors on `pin/2026-10-23`, 195 here. The three new ones are the same `Property '…' does not exist on type 'Ref<APISummary | undefined, …>'` that upstream's own uses of `status` in those two templates already get.
- Backend, with Go 1.27.3 (`go.mod` asks for 1.27.0): `.output/public` copied to `backend/app/api/static/public` as the `Dockerfile` does, then `CGO_ENABLED=0 go build -ldflags "-s -w -X main.commit= -X main.buildTime= -X main.version=" ./app/api/*.go` succeeded. `gofmt -l` is clean on the changed Go files, `go vet ./app/api/handlers/v1/ ./internal/sys/config/` passes, and `go test ./internal/sys/config/...` passes.
- Run as the container would: a scratch PostgreSQL 14 on 127.0.0.1, the image's `ENV` defaults, `PORT` and `DATABASE_URL=postgresql://homebox:<password>@127.0.0.1:<port>/homebox?sslmode=disable`, no pepper set, started through this pin's `tofu-entrypoint.sh` with `/app/api` pointed at the local binary. The server started, so the derived pepper was accepted; goose migrated the database to `20260821000000`. `GET /` answered 200 (`text/html`) and `GET /api/v1/status` answered 200 with `"sourceUrl":"https://github.com/tofu-gallery/homebox/tree/pin/2026-10-23.1"` and an empty build version, commit and time.
- API keys and the pepper: registered a user, signed in and created an API key; `GET /api/v1/users/self` answered 200 with the key and with the session. Then the database role's password was changed and the server restarted with the new `DATABASE_URL`: the same key got `401 {"error":"valid authorization token is required"}`, the session still got 200, and the key was still listed.
- Without `DATABASE_URL` and without a pepper, the entrypoint's `/app/api` exited with status 2 and `auth.api_key_pepper must be set to at least 32 bytes`.
- Browser (Aside, own tab, 127.0.0.1 only), with `HBOX_DEMO=true` added so the sign-in form fills itself: the sign-in page's GitHub icon links to https://github.com/tofu-gallery/homebox/tree/pin/2026-10-23.1 in a new tab; signing in reached `/home`, whose footer reads "GitHub Project ~ API" with the same address; creating the item "Tofu verification lamp" in the Attic opened its item page with the notice "Item created". No fetch request failed. The console showed `Failed to set theme TypeError: Cannot read properties of null (reading 'theme')` once on a first load, and `websocket error` while the server answered two `/api/v1/ws/events` attempts with 403 during sign-in before later ones upgraded; neither comes from Tofu's changes.
- Import filter: see the line below.
