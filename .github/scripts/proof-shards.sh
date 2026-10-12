#!/bin/bash
# Splits the tests of internal/proof into shards for the race jobs of CI.
# Each shard is one -run pattern. Each pattern names whole tests, so that
# `go test -list` shows the same tests that `go test -run` runs. The last
# shard, other, has no pattern. It runs each test that no other shard runs,
# so a new proof test runs there until you move it.
#
#     .github/scripts/proof-shards.sh names      # prints the shard names
#     .github/scripts/proof-shards.sh run NAME   # runs the tests of NAME
#     .github/scripts/proof-shards.sh check      # checks the shards
#
# The check fails if a test of internal/proof is in more than one shard, or
# if a shard has no test. It prints the tests of the shard other.
#
# Arguments after NAME go to `go test`. GOFLAGS and GORACE come from the
# environment.
set -euo pipefail

# Each line is a shard name and its pattern. The shard other is not here.
shards='
delivery ^TestDelivery$
interface ^TestInterface$
storage-pieces ^Test(StoragePieces|StorageFormat1)$
storage-repair ^Test(StorageRepair|StorageRepairTURN|StorageSealsWithDataRoots)$
storage-sync ^Test(StorageNetworkSync|StorageJournalSync|StorageList)$
'

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
cd "$root"

names() {
  awk 'NF { print $1 }' <<<"$shards"
}

# others is one pattern that matches each test of the named shards.
others() {
  awk 'NF { print $2 }' <<<"$shards" | paste -sd '|' -
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
  echo other
  ;;
run)
  name="${2:?usage: $0 run NAME [go test flags]}"
  if [ "$name" = other ]; then
    shift 2
    go test -count=1 -timeout 40m "$@" -skip "$(others)" ./internal/proof/
    exit
  fi
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
  twice="$(grep . <<<"$sharded" | sort | uniq -d)"
  if [ -n "$twice" ]; then
    echo "these proof tests are in more than one shard of $0:" >&2
    echo "$twice" >&2
    exit 1
  fi
  other="$(comm -23 <(sort <<<"$all") <(sort <<<"$sharded" | grep .))"
  if [ -z "$other" ]; then
    echo "shard other matches no test" >&2
    exit 1
  fi
  echo "$(wc -l <<<"$all" | tr -d ' ') proof tests, each in one of $(($(names | wc -l) + 1)) shards"
  echo "shard other runs:"
  echo "$other"
  ;;
*)
  sed -n '2,14p' "$0"
  exit 2
  ;;
esac
