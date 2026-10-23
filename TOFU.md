# Tofu gallery copy

This repository is Tofu's copy of **sysadminsmedia/homebox** (licence: AGPL-3.0), listed in Tofu's app gallery as **Homebox**.

- `main` mirrors the upstream repository and is never committed to by Tofu.
- `tofu` carries Tofu's own changes; tags named `pin/<date>` mark the versions the gallery points to.
- A visitor who chooses this app gets a private snapshot of the pinned version in their own Tofu account. Their copy does not follow upstream updates.

How Tofu runs it: Go server with a Dockerfile; PostgreSQL supplied through environment variables.

## What Tofu changes

- `tofu-entrypoint.sh` (new). The image now starts through this script. It does two things, then runs `/app/api` with the same arguments:
  - It sets `HBOX_WEB_PORT` to Tofu's `PORT`. Tofu sends traffic to `PORT`, and Homebox would otherwise listen on 7745. Without `PORT`, the upstream default of 7745 stays.
  - When `DATABASE_URL` is set, it splits it into the fields Homebox reads: `HBOX_DATABASE_DRIVER=postgres`, `HBOX_DATABASE_HOST`, `HBOX_DATABASE_PORT`, `HBOX_DATABASE_USERNAME`, `HBOX_DATABASE_PASSWORD`, `HBOX_DATABASE_DATABASE` and `HBOX_DATABASE_SSL_MODE`. Homebox has no setting that takes a URL. Any `HBOX_DATABASE_*` value the owner sets is kept.
- `Dockerfile`:
  - `ENTRYPOINT` is now `/bin/sh /app/tofu-entrypoint.sh`, and the script is copied into the image. The `CMD` is unchanged.
  - `HBOX_THUMBNAIL_ENABLED=false` and `HBOX_OPTIONS_GITHUB_RELEASE_CHECK=false` are now image defaults, and the owner can set either back to `true`. Thumbnails cost image processing for photos that are lost on restart anyway (see below). The release check calls GitHub every hour to show an update notice, but this copy is a fixed snapshot.
  - `HEALTHCHECK` checks the port the server actually listens on (`PORT`, then `HBOX_WEB_PORT`, then 7745), not a fixed 7745.
- `backend/app/api/setup.go`: when `DATABASE_URL` is set, the Postgres connection adds `default_query_exec_mode=cache_describe`. Tofu's `DATABASE_URL` goes through a transaction pooler, which keeps no named prepared statements, and those are pgx's default. With this mode, every query runs on the unnamed statement.

## Before the first deploy

- Set `HBOX_AUTH_API_KEY_PEPPER` as a hidden value on the app's Keys & settings page. It must be at least 32 bytes, for example the output of `openssl rand -base64 48`. Homebox will not start without it, and Tofu does not set it. Keep it unchanged afterwards: changing it cancels every API key the app has issued. Sign-in sessions are not affected.

## What works differently on Tofu

- **Item photos and attachments need a storage driver.** They are written to `/data` inside the container (`HBOX_STORAGE_CONN_STRING=file:///?no_tmp_dir=true`), and that folder is lost on every restart and deploy. To keep them, point `HBOX_STORAGE_CONN_STRING` at a bucket and give the app that bucket's keys as hidden values. The image already includes the `s3://`, `gs://` and `azblob://` drivers.
- **Live refresh over a websocket is off.** A page does not update by itself when something changes in another tab or on another device; reload it to see the change. The browser console may show the page retrying `/api/v1/ws/events`.
- **Maintenance reminders need a trigger.** The running server checks once an hour. During the 08:00 hour (UTC in this image), it sends the day's scheduled maintenance through the notifiers the owner set up. If the app is asleep or restarting at that hour, nothing is sent that day. Reliable reminders need something that wakes the app on a schedule.
- **Homebox has its own login, not Tofu sign-in.** People create a Homebox account on the app's own page. Anyone who can open the address can register until the owner sets `HBOX_OPTIONS_ALLOW_REGISTRATION=false` after creating their own account, or makes the app private on Tofu.

## Needs a real deploy to verify

- The image builds on Tofu's container hosting. It is a multi-stage build: the frontend with Node 22 and pnpm, the backend with Go. The Go step uses a BuildKit cache mount (`RUN --mount=type=cache`).
- The server answers `/` and `/api/v1/status` on `PORT`, and the log says `Server is running on :<PORT>`.
- The first start connects through `DATABASE_URL` and runs all the goose migrations against the pooler.
- Sign-up, sign-in, and creating, editing, searching and deleting items all work. The runtime logs show no `prepared statement` errors (SQLSTATE 26000 or 42P05).
- An uploaded item photo shows until the next restart, which shows `/data` is writable in Tofu's container.
- Whether `/api/v1/ws/events` upgrades to a websocket on Tofu at all.
- Whether sign-in rate limiting sees each visitor's own address. Behind Tofu's proxy, every visitor may share one address. Then five failed sign-ins from anyone would slow sign-in for everyone, and five sign-ups a minute would be the limit for the whole app. `HBOX_OPTIONS_TRUST_PROXY=true` reads the forwarded address instead, but only set it if Tofu's proxy sets that address and visitors cannot forge it.

## Licence

AGPL-3.0. Licence files and copyright notices stay as they are; every file Tofu changes carries a note saying so.
