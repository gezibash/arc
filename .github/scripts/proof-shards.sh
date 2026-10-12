#!/bin/bash
# Splits the tests of internal/proof into shards for the race jobs of CI.
# Each shard is one -run pattern. Each pattern names whole tests, so that
# `go test -list` shows the same tests that `go test -run` runs.
#
#     .github/scripts/proof-shards.sh names      # prints the shard names
#     .github/scripts/proof-shards.sh run NAME   # runs the tests of NAME
#     .github/scripts/proof-shards.sh check      # checks the shards
#
# The check fails if a test of internal/proof is in no shard, or in more than
# one shard. Add each new proof test to one shard.
#
# Arguments after NAME go to `go test`. GOFLAGS and GORACE come from the
# environment.
set -euo pipefail

# Each line is a shard name and its pattern.
shards='
delivery ^TestDelivery$
interface ^TestInterface$
storage-pieces ^Test(StoragePieces|StorageFormat1)$
storage-repair ^Test(StorageRepair|StorageRepairTURN)$
storage-sync ^Test(StorageNetworkSync|StorageJournalSync|StorageList)$
other ^Test(Devices|MessageWatch|ExecKeep|DataRoots|Seed|StorageSealsWithDataRoots|StorageImportsDrafts)$
'

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
cd "$root"

names() {
  awk 'NF { print $1 }' <<<"$shards"
}

pattern() {
  awk -v name="$1" '$1 == name { print $2 }' <<<"$shards"
}

list() {
  go test -list "$1" ./internal/proof/ | grep '^Test' || true
}

case "${1:-}" in
names)
  names
  ;;
run)
  name="${2:?usage: $0 run NAME [go test flags]}"
  pattern="$(pattern "$name")"
  if [ -z "$pattern" ]; then
    echo "no shard $name" >&2
    exit 2
  fi
  shift 2
  go test -count=1 -timeout 40m "$@" -run "$pattern" ./internal/proof/
  ;;
check)
  all="$(list .)"
  sharded=""
  for name in $(names); do
    tests="$(list "$(pattern "$name")")"
    if [ -z "$tests" ]; then
      echo "shard $name matches no test" >&2
      exit 1
    fi
    sharded+="$tests"$'\n'
  done
  missing="$(comm -23 <(sort <<<"$all") <(sort -u <<<"$sharded" | grep .))"
  twice="$(grep . <<<"$sharded" | sort | uniq -d)"
  if [ -n "$missing" ]; then
    echo "these proof tests are in no shard of $0:" >&2
    echo "$missing" >&2
  fi
  if [ -n "$twice" ]; then
    echo "these proof tests are in more than one shard of $0:" >&2
    echo "$twice" >&2
  fi
  if [ -n "$missing$twice" ]; then
    exit 1
  fi
  echo "$(wc -l <<<"$all" | tr -d ' ') proof tests, each in one of $(names | wc -l | tr -d ' ') shards"
  ;;
*)
  sed -n '2,14p' "$0"
  exit 2
  ;;
esac
