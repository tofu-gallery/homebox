# Tofu gallery copy

This repository is Tofu's copy of **sysadminsmedia/homebox** (licence: AGPL-3.0), listed in Tofu's app gallery as **Homebox**.

- `main` mirrors the upstream repository and is never committed to by Tofu.
- `tofu` carries Tofu's own changes; tags named `pin/<date>` mark the versions the gallery points to.
- A visitor who chooses this app gets a private snapshot of the pinned version in their own Tofu account. Their copy does not follow upstream updates.

How Tofu runs it: Go server with a Dockerfile; PostgreSQL supplied through environment variables.

## What Tofu changes

Nothing yet.

## Licence

AGPL-3.0. Licence files and copyright notices stay as they are; every file Tofu changes carries a note saying so.
