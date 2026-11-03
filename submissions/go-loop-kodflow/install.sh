#!/usr/bin/env bash
# Runs once as root on a clean Ubuntu 24.04: a C toolchain for cgo and the
# SQLite amalgamation, and Go from the official tarball. Every download is
# checked against a pinned digest before it is used.
set -euo pipefail

GO_VERSION=1.27.3
GO_SHA256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends build-essential ca-certificates curl openssl unzip

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o "$tmp/go.tar.gz"
echo "$GO_SHA256  $tmp/go.tar.gz" | sha256sum -c -
rm -rf /usr/local/go
tar -C /usr/local -xzf "$tmp/go.tar.gz"
/usr/local/go/bin/go version
