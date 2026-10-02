#!/usr/bin/env bash
# Fail, naming docs/dev/rauc-signing.md, when no OS release can be signed
# because dev/release/rauc/release-ca.pem is missing, is not a CA, has expired,
# or is a throwaway (#562, dev/cloud/rauc.sh rauc_require_release_ca).
#
# release.yml runs it before it tags or creates any Release for a merge that
# bumps VERSION, so an OS release cut before the maintainer's key setup stops
# with nothing tagged, on either line. ci-cloud-image.yml runs it first on any
# run that publishes the OS line.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=dev/cloud/rauc.sh
. "${REPO_ROOT}/dev/cloud/rauc.sh"
if ! msg="$(rauc_require_release_ca 2>&1)"; then
    echo "::error::${msg}"
    exit 1
fi
echo "release root CA: $(openssl x509 -in "$RAUC_RELEASE_CA" -noout -subject -enddate | tr '\n' ' ')"
