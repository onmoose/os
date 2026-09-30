#!/usr/bin/env bash
# SPIKE (#485), not for merge. Builds systemd-sysupdate for Debian trixie.
#
# Debian builds systemd with -Dsysupdate=disabled, so trixie has no
# systemd-sysupdate binary at all. This builds one from the same upstream
# tag as trixie's systemd (257.13). systemd 257 has no standalone target
# for sysupdate, so we add one the same way repart.standalone is defined:
# it links systemd's shared code statically. The binary then does not need
# Debian's private libsystemd-shared-257.so, and a Debian systemd security
# update cannot break it by changing that library.
#
# Runs inside a debian:trixie container. Output: $OUT/systemd-sysupdate
set -euo pipefail
OUT="${1:?usage: build-sysupdate.sh <out-dir>}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
TAG=v257.13

docker run --rm -v "$OUT:/out" debian:trixie bash -euxc "
apt-get update -qq
apt-get install -y -qq --no-install-recommends git ca-certificates meson ninja-build gcc gperf pkg-config \
    python3-jinja2 libcap-dev libmount-dev libblkid-dev libfdisk-dev libssl-dev libcrypt-dev >/dev/null
git clone -q --depth=1 --branch $TAG https://github.com/systemd/systemd.git /src
cd /src
cat >> src/sysupdate/meson.build <<'MESON'

executables += [
        executable_template + {
                'name' : 'systemd-sysupdate.standalone',
                'public' : true,
                'conditions' : ['ENABLE_SYSUPDATE'],
                'sources' : systemd_sysupdate_sources,
                'c_args' : '-DSTANDALONE',
                'link_with' : [
                        libbasic_static,
                        libshared_fdisk,
                        libshared_static,
                        libsystemd_static,
                ],
                'dependencies' : [
                        libblkid,
                        libfdisk,
                        libopenssl,
                        threads,
                ],
                'build_by_default' : false,
                'install' : false,
        },
]
MESON
meson setup build -Dmode=release -Dauto_features=disabled -Dsysupdate=enabled \
    -Dfdisk=enabled -Dblkid=enabled -Dopenssl=enabled -Dlibcryptsetup=disabled \
    -Dtests=false -Dman=disabled -Dhtml=disabled >/dev/null
ninja -C build systemd-sysupdate.standalone
cp build/systemd-sysupdate.standalone /out/systemd-sysupdate
/out/systemd-sysupdate --version
ldd /out/systemd-sysupdate
"
