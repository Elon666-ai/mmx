#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-dev}"
BUILD_TIME="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

mkdir -p bin
go build \
  -ldflags "-s -w -X 'main.Version=${VERSION}' -X 'main.BuildTime=${BUILD_TIME}' -X 'main.GitCommit=${GIT_COMMIT}'" \
  -o bin/mmx.exe \
  main.go
