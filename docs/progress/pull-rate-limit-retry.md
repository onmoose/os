# Retry an image pull the registry rate-limits

- **Status:** done
- **Date:** 2026-10-07
- **Specs touched:** `docs/specs/APP_LIFECYCLE.md`

A catalog install on a hosted box failed at `resolving_digests` because ghcr answered one `docker pull` with HTTP 429 (`toomanyrequests: retry-after: 25.51µs, allowed: 44000/minute`). A box pulls without logging in, so the registry counts its requests against a source IP that other traffic may share. The limit was a short spike: installing again a minute later worked. The pull ran once, so one 429 rolled the whole install back (#586).

## What was done

- **`pullWithRetry`** in `internal/lifecycle/pinning.go` wraps both pulls in `pullAndResolve` (the Door-1 digest pull and the Door-2 tag pull). When the pull error is a rate limit, it waits and pulls again: 2, 4, 8 and 16 seconds, so four retries over about 30 seconds. It logs a `slog.Warn` for each retry, so a box that keeps hitting the limit shows it. It stops when the install context ends, and then the error wraps `ctx.Err()`, so a cancelled install does not read as a rate limit.
- **`isRateLimited`** decides what counts: the `docker pull` output holds `toomanyrequests: ` at a line start or after a space (the OCI error code ghcr and Docker Hub both send), or `429 Too Many Requests`. An image reference has no spaces and no capitals, so neither form can match inside one. The driver's own `pull <ref>: ` prefix is cut first. Every other pull error returns at once, so an unreachable registry still fails fast and the offline fallback (`resolveOffline`) still engages without waiting. A rate limit means a registry answered, so an offline-mode box retries it like any other box before it falls back.
- **Tests** in `internal/lifecycle/pinning_retry_test.go`: an install whose pull is rate-limited twice and then served succeeds; a pull that stays rate-limited fails with the registry's error after the last retry; a non-rate-limit error is pulled once; a context cancelled before the call, and one cancelled during a wait, both stop at once; and a table for `isRateLimited`, including short image names that appear inside the error text and an image whose name is `toomanyrequests`. The fake driver gains `pullFails`/`pullFailErr` to fail the next N pulls.

## How it maps to the specs

`APP_LIFECYCLE.md` # Locked: image digest pinning now says a rate-limited pull is retried before the install fails. The install transaction is unchanged: a pull that still fails after the last retry rolls back as before.

## Known gaps & deviations

- The registry's `retry-after` hint is not read. ghcr sends microseconds and Docker Hub often sends none, so a fixed backoff is simpler and waits at least as long.
- Other transient pull errors (a 5xx, a TLS timeout) are not retried. Nothing has hit them yet.
- The control-plane update pull (`internal/hostagent/cpupdate`) has its own path and is not changed here. A failed update there is retried at the next update window.
- Each image gets its own retries, one image after another. Retry waits can add up across images that end up served. If an image stays rate-limited through all four retries, the install fails after about 30 seconds of waits for that image, and later images are not pulled. The install context still bounds it.
- Not tested against a real rate-limiting registry: a 429 cannot be produced on demand. The match is on the error text the real failure printed.

## What's next

Nothing. If 429s keep showing in the `image pull rate-limited` log line, the next step is an authenticated pull.
