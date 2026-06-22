# PodSync Go fork - orientation for Claude

A fork of mxpv/podsync. Two bespoke changes on top of upstream:
1. **Alpine 3.24 / Deno** in the `Dockerfile` - newer Deno solves YouTube's `n`-challenge
   (EJS) so yt-dlp can fetch real formats, not just images. Do not downgrade the base.
2. **The strip-ads publish barrier** (the reason this fork exists right now).

## The strip-ads barrier
The host stack (`~/git/podsyncfixdocker`) strips ads from feeds marked `strip_ads.enabled`.
This fork is the IN-CONTAINER half: it diverts a strip-ads feed's raw download to unserved
staging + enqueues a job, and publishes only the host daemon's CUT result. The uncut file
never enters the RSS feed.

- `services/update/adstrip.go` - the barrier: the job/result contract structs, path
  sanitisation, `divert()` (stage raw + write job), `reconcile()` (publish/hold from the
  result file), the `adstripStore` interface. Injectable store + clock for hermetic tests.
- `services/update/updater.go` - wired in: a strip-ads feed diverts (instead of `fs.Create`
  into the served tree) and marks `EpisodeStripProcessing`; `reconcileStripAds()` runs at the
  start of each `Update()` and publishes/holds processing episodes (NON-blocking - it does not
  wait on the serial host worker).
- `pkg/model/feed.go` - the non-RSS statuses `EpisodeStripProcessing` / `EpisodeStripFailed`.
  `pkg/feed/xml.go` gates RSS on `== EpisodeDownloaded`, so both are auto-excluded.
- `pkg/feed/config.go` + `cmd/podsync/config.go` - the `StripAds` feed config + Normalize().

The host<->container JSON contract is `~/git/podsyncfixdocker/design/adstrip-job-result-contract.md`.
Key invariants: the barrier publishes ONLY `done_cut`/`done_no_cuts` AND only when the result's
`outputPath` is exactly the `<ytid>.cut.mp4` it re-derives; an `attempt` token rejects a stale
result from a crashed-then-retried attempt; raw is NEVER copied into the served tree.

## Build / deploy (this is the bit that breaks)
The live image is built FROM THIS REPO. To ship a change:
```
docker build -t podsync-stripads-<newtag> .          # new tag; keep the old as rollback
```
then point `~/git/podsyncfixdocker/docker-compose.stripads.yml` at the new tag and recreate
WITH BOTH compose files:
```
cd ~/git/podsyncfixdocker
docker compose -f docker-compose.yml -f docker-compose.stripads.yml up -d --build podsync
```
Current live image: `podsync-stripads-jun22`. Rollback image: `podsync-handles-alpine324`
(the base compose still references it, so a bare `docker compose up -d` reverts the barrier -
do not do that by accident). The barrier is INERT for any feed without `strip_ads.enabled`,
so existing feeds are byte-identical.

`go build ./...` + `go test ./services/update/... ./pkg/feed/... ./cmd/podsync/...` must pass.
