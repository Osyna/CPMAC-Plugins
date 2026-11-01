#!/bin/sh
# Builds plugins/token-trim.so inside Debian bookworm so its glibc matches the cli-proxy-api image.
set -eu
cd "$(dirname "$0")"
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false -e GOCACHE=/tmp/gocache golang:1.26-bookworm \
	go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o /src/token-trim.so .
rm -f token-trim.h
mkdir -p ../plugins
mv token-trim.so ../plugins/token-trim.so
echo "built ../plugins/token-trim.so"
