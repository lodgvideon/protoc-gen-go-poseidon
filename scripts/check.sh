#!/usr/bin/env bash
# The five-module gate, in one place and STOPPING ON THE FIRST FAILURE.
#
# It exists because the shell loop it replaces did not stop. A
#   for m in ...; do (cd "$m" && ...); done
# prints each module's result and then exits 0 regardless, so a commit went in
# with an outstanding lint finding that only the next run caught. A loop that
# cannot fail is not a gate.
#
# It is a script rather than a Makefile because make is not installed on the
# maintainer's machine, and a gate nobody can run locally is the same mistake
# in a nicer hat.
#
# Six modules, because each proves something the others cannot:
#   .                 the plugin binary; requires only google.golang.org/protobuf
#   pgrpc             the runtime that generated code calls into
#   testdata          compiles the generated output against pgrpc and NOTHING else
#   test/e2e          drives that output against a real grpc-go server
#   examples/loadgen  a consumer-shaped module; its dependency graph is a gate
#   examples/service  the ordinary case: one RPC per request, faked in tests
#
# Usage: scripts/check.sh [fmt|vet|test|race|lint|deps]...   (default: all but race)
set -euo pipefail

cd "$(dirname "$0")/.."
MODULES=(. pgrpc testdata test/e2e examples/loadgen examples/service)

step_fmt() {
  local bad=0
  for m in "${MODULES[@]}"; do
    local out
    out=$(cd "$m" && gofmt -l .)
    if [ -n "$out" ]; then echo "gofmt: $m: $out"; bad=1; fi
  done
  [ "$bad" -eq 0 ] || return 1
  echo "fmt ok"
}

step_vet() {
  for m in "${MODULES[@]}"; do (cd "$m" && go build ./... && go vet ./...); done
  echo "vet ok"
}

step_test() {
  for m in "${MODULES[@]}"; do (cd "$m" && go test ./... -count=1); done
}

# What CI actually gates on. step_test is the fast local form.
step_race() {
  for m in "${MODULES[@]}"; do (cd "$m" && go test ./... -count=1 -race); done
}

step_lint() {
  for m in "${MODULES[@]}"; do (cd "$m" && golangci-lint run ./...); done
  echo "lint ok"
}

# The claim that a user's binary links the runtime, not the plugin. A claim in
# a README is worth nothing without a check.
step_deps() {
  local deps
  deps=$(cd examples/loadgen && go list -deps ./...)
  for p in google.golang.org/protobuf/compiler/protogen \
           google.golang.org/protobuf/types/pluginpb \
           google.golang.org/grpc; do
    if grep -qx "$p" <<<"$deps" || grep -q "^$p/" <<<"$deps"; then
      echo "deps: $p is linked into the example binary"
      return 1
    fi
  done
  echo "deps ok: no compiler, no pluginpb, no grpc-go"
}

steps=("$@")
[ "${#steps[@]}" -gt 0 ] || steps=(fmt vet test lint deps)
for s in "${steps[@]}"; do "step_$s"; done
echo "check ok"
