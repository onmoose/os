# The bundle build fetches RAUC over https, with a fixed apt

- **Status:** done
- **Date:** 2026-11-07
- **Specs touched:** `docs/specs/BUILD.md` (# 1b # The bundle: what a box downloads, "How it is built").

Follows [review-agent-brief.md](review-agent-brief.md). Closes #645. Debian fixed an apt bug on 2026-11-04 (DSA-6607-1, CVE-2026-109518, fixed in trixie in apt 3.0.4). When apt retries a download (`Acquire::Retries`), anyone who can change plain-http traffic could make it install a package it never verified, as root. `rauc_run` in `dev/cloud/rauc.sh` met every condition: apt 3.0.3 from the pinned `debian:trixie-slim`, `Acquire::Retries=5`, and `http://snapshot.debian.org`. That container builds the OS bundle, and in the `sign` job (`dev/release/sign-bundle.sh`) it holds the release signer.

## What was done

- **`BRAIN_RUNTIME_IMAGE`** in `dev/control-plane/images.lock` moves to `debian:trixie-slim@sha256:a72b4433…` (Debian 13.7, built 2026-11-04), which has apt 3.0.4. The same pin is the brain image's runtime base. The brain never runs apt at run time, so that copy was low risk, but it moves too.
- **`rauc_run` fetches over https.** The slim image has no CA store, so it mounts the build host's CA bundle read-only (`/etc/ssl/certs/ca-certificates.crt`, or `/etc/pki/tls/certs/ca-bundle.crt`) and points apt at it with `Acquire::https::CaInfo`. Without that setting OpenSSL finds no trust store in the image and every fetch fails. With no bundle on the host, `rauc_run` stops with an error. It never falls back to http.
- **Tested** by running `rauc_run` locally against the locked snapshot: apt 3.0.4, all three sources on https, rauc 1.13 installed. `make brain-image` builds on the new base, and the pin test passes. `CI / Cloud image` (publish=false) runs on the PR.

## Known gaps

- The brain image's runtime stage still `apt-get`s `docker-ce-cli` from Docker's live https index, as before (`BUILD.md` # 5c). This change does not touch that.
- The main image build already used `https://snapshot.debian.org` (`dev/os-lock/os-lock.sh`) and is not changed.

## What's next

- Nothing.
