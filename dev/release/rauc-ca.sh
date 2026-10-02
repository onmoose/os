#!/usr/bin/env bash
# Make the RAUC root CA and issue bundle signers from it (#562,
# docs/dev/rauc-signing.md, DECISIONS.md 2026-10-02 key custody).
#
#   rauc-ca.sh root   DIR        make DIR/root-ca.key and DIR/root-ca.pem
#   rauc-ca.sh signer DIR NAME   issue DIR/NAME.key and DIR/NAME.pem from the root in DIR
#   rauc-ca.sh show   FILE.pem   print a certificate's subject, dates and purpose
#
# The maintainer runs `root` and `signer` OFFLINE, on a machine that is not
# CI. The root key never leaves that machine: it is written encrypted
# (AES-256), and openssl asks for its passphrase. root-ca.pem is public: it is
# committed as dev/release/rauc/release-ca.pem and baked into every OS image as
# /etc/rauc/keyring.pem. A signer's key and cert go into the `os-release`
# GitHub Environment as RAUC_SIGNING_KEY and RAUC_SIGNING_CERT.
#
# CI uses the same script for the throwaway CA of a run that publishes no OS
# (dev/cloud/rauc.sh), with RAUC_CA_NO_PASSPHRASE=1 and short lifetimes. That
# is the only reason the knobs below exist; never set them for the real root.
#
# Knobs (environment):
#   RAUC_CA_NAME          the CN prefix          (default "moose OS release")
#   RAUC_CA_ROOT_DAYS     the root's lifetime    (default 7300, 20 years)
#   RAUC_CA_SIGNER_DAYS   a signer's lifetime    (default 1095, 3 years)
#   RAUC_CA_NO_PASSPHRASE 1 writes the root key unencrypted (throwaway only)
#
# Keys are EC P-256. A signer carries the codeSigning purpose, which the image's
# /etc/rauc/system.conf asks for (check-purpose=codesign).
set -euo pipefail

name="${RAUC_CA_NAME:-moose OS release}"
root_days="${RAUC_CA_ROOT_DAYS:-7300}"
signer_days="${RAUC_CA_SIGNER_DAYS:-1095}"
umask 077

# A failed run removes what it made, so the next run can start again (a root
# key left behind without its cert would make `root` refuse the directory).
created=()
cleanup() {
    local rc=$?
    if [ "$rc" -ne 0 ] && [ "${#created[@]}" -gt 0 ]; then
        rm -f -- "${created[@]}"
        echo "rauc-ca: failed; removed the partial files: ${created[*]}" >&2
    fi
    exit "$rc"
}
trap cleanup EXIT

ext() { # the x509v3 extensions both certificates use
    cat <<'EOF'
[root]
basicConstraints=critical,CA:true,pathlen:0
keyUsage=critical,keyCertSign,cRLSign
subjectKeyIdentifier=hash
[signer]
basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid
EOF
}

cmd="${1:-}"
case "$cmd" in
root)
    dir="${2:?usage: rauc-ca.sh root DIR}"
    mkdir -p "$dir"
    if [ -e "$dir/root-ca.key" ] || [ -e "$dir/root-ca.pem" ]; then
        echo "rauc-ca: $dir already holds a root; refusing to overwrite it" >&2
        exit 1
    fi
    enc=(-aes256)
    if [ "${RAUC_CA_NO_PASSPHRASE:-}" = "1" ]; then enc=(); fi
    created=("$dir/root-ca.key" "$dir/root-ca.pem")
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 "${enc[@]}" -out "$dir/root-ca.key"
    openssl req -x509 -new -key "$dir/root-ca.key" -sha256 -days "$root_days" \
        -subj "/CN=${name} root CA $(date -u +%Y)" \
        -config <(printf '[req]\ndistinguished_name=dn\n[dn]\n'; ext) -extensions root \
        -out "$dir/root-ca.pem"
    chmod 0644 "$dir/root-ca.pem"
    echo "root CA: $dir/root-ca.pem (public; commit it as dev/release/rauc/release-ca.pem)"
    echo "root key: $dir/root-ca.key (secret; keep it offline)"
    ;;
signer)
    dir="${2:?usage: rauc-ca.sh signer DIR NAME}"
    sname="${3:?usage: rauc-ca.sh signer DIR NAME}"
    case "$sname" in *[!A-Za-z0-9._-]*|"") echo "rauc-ca: signer NAME may use only letters, digits, . _ -" >&2; exit 2 ;; esac
    [ -f "$dir/root-ca.key" ] && [ -f "$dir/root-ca.pem" ] || { echo "rauc-ca: no root in $dir; run rauc-ca.sh root $dir first" >&2; exit 1; }
    if [ -e "$dir/$sname.key" ] || [ -e "$dir/$sname.pem" ]; then
        echo "rauc-ca: $dir already holds a signer called $sname; pick a new name" >&2
        exit 1
    fi
    csr="$(mktemp)"
    created=("$dir/$sname.key" "$dir/$sname.pem" "$csr")
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$dir/$sname.key"
    openssl req -new -key "$dir/$sname.key" -subj "/CN=${name} signer ${sname}" -out "$csr"
    openssl x509 -req -in "$csr" -CA "$dir/root-ca.pem" -CAkey "$dir/root-ca.key" -sha256 \
        -set_serial "0x$(openssl rand -hex 16)" -days "$signer_days" \
        -extfile <(ext) -extensions signer -out "$dir/$sname.pem"
    chmod 0644 "$dir/$sname.pem"
    rm -f "$csr"
    # openssl 3.2 and later know the codesign purpose; older ones check the
    # chain, and the purpose is then read from the cert itself.
    if ! openssl verify -purpose codesign -x509_strict -CAfile "$dir/root-ca.pem" "$dir/$sname.pem" 2>/dev/null; then
        openssl verify -x509_strict -CAfile "$dir/root-ca.pem" "$dir/$sname.pem"
        openssl x509 -in "$dir/$sname.pem" -noout -ext extendedKeyUsage | grep -q "Code Signing" \
            || { echo "rauc-ca: $dir/$sname.pem lacks the codeSigning purpose" >&2; exit 1; }
    fi
    echo "signer cert: $dir/$sname.pem (the RAUC_SIGNING_CERT secret)"
    echo "signer key: $dir/$sname.key (the RAUC_SIGNING_KEY secret; delete this copy once it is stored)"
    ;;
show)
    f="${2:?usage: rauc-ca.sh show FILE.pem}"
    openssl x509 -in "$f" -noout -subject -issuer -startdate -enddate -ext basicConstraints,extendedKeyUsage
    ;;
*)
    sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
