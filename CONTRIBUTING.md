# Contributing to Diaspore

Diaspore is an amnesiac, roaming mobile OS built on LineageOS: the OS image is
local and verified; only the user's encrypted state roams through a
content-addressed store and is wiped from the device at power-off. Keep that
framing exact — Diaspore roams *user state*, it does **not** netboot the OS.

This public repository is a clean, source-only mirror of the OS. Please keep
contributions scoped to the Diaspore OS itself: the roaming agent, the chooser
gate, the login daemon, the `su:s0` data worker, init/sepolicy, the build
overlay, and the docs.

## Workflow

- **Work on a branch — never commit directly to `main`.** Branch off `main`,
  open a pull request, and let a maintainer merge. This applies to small fixes
  too.
- Name the branch `dia-YYYYMMDD-NN-<short-topic>` for a tracked slice, or
  `codex/<short-topic>` for a small no-ID cleanup.
- Material work gets a `DIA-YYYYMMDD-NN` work ID; its commit subjects start with
  the ID (`DIA-YYYYMMDD-NN: <summary>`). Keep subjects imperative and specific.
- End AI-assisted commit messages with a `Co-Authored-By` trailer.
- Leave the touched surface review-ready: no placeholders (`TBD`, `TODO`,
  `FIXME`), no stale status, no `*.img`/`out/`/log artifacts staged.

## Security (read first — this project is full of secrets)

- **Never commit store credentials, keys, or device config.** The store endpoint
  plus access/secret keys live in `core/vendor-common/etc/diaspore.conf`, which
  is provisioned out-of-band and git-ignored. There is no `diaspore.conf` in the
  tree and there must never be one.
- Profile passphrases and session credentials are runtime-only (RAM / tmpfs).
  They must not be baked into the image, committed, or logged.
- The **blind-login** property is load-bearing: a wrong passphrase or unknown
  profile must resolve to an indistinguishable blank. Do not add UI, logging,
  timing, or error paths that leak which profiles exist.
- Run `git diff --check` and re-read your diff for secrets before every commit.

## Building & verifying

- **Agent (Go):** from `core/agent`, build the static device binary with
  `GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o diaspore_agent .`.
  For a quick check: `go build ./...`, `go vet ./...`, `go test ./...`. CI runs
  these on every push.
- **OS image:** the full LineageOS image builds on a build host with a synced
  LineageOS tree, not in CI. Assemble the single `vendor/diaspore` tree with
  `editions/diaspore/build/stage-vendor.sh <repo> <tree>/vendor/diaspore`, then
  `m systemimage vbmetaimage`. Flash + on-device validation happen on a
  Fairphone 3.
- **Don't claim a device behavior works until it is verified on hardware** when
  the change touches the device-owner lifecycle, sepolicy, FBE, multi-user
  roaming, or the boot/login/logout flow.

## Where things live

- `README.md` — vision, the defining loop, and entry points.
- `docs/` — architecture and behavior specs (design, roadmap, app model,
  storage, enrollment, boot flow, prior art).
- `core/` — the OS- and device-independent pieces shared across editions (the Go
  agent, the chooser gate, the shared system-side vendor pieces).
- `editions/diaspore/` — the Fairphone / LineageOS integration (the RRO overlay,
  the device `.mk`, and the build glue).
