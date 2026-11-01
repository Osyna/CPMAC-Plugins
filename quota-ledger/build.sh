#!/bin/sh
# Builds plugins/quota-ledger.so inside Debian bookworm so its glibc matches the cli-proxy-api image.
set -eu
cd "$(dirname "$0")"
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false -e GOCACHE=/tmp/gocache golang:1.26-bookworm \
	go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o /src/quota-ledger.so .
rm -f quota-ledger.h
mkdir -p ../plugins
mv quota-ledger.so ../plugins/quota-ledger.so
echo "built ../plugins/quota-ledger.so"
