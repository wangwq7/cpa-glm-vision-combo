#!/bin/sh
set -eu

echo "[1/2] regular tests"
go test ./...
echo "[2/2] build shared library (race tests run on the development host)"
make build-linux-amd64
sha256sum dist/glm-vision-combo.so
