#!/usr/bin/env bash
set -u

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <report-root> <test-id> <command...>" >&2
  exit 2
fi

report_root=$1
test_id=$2
shift 2

test_dir="$report_root/$test_id"
mkdir -p "$test_dir"

set +e
"$@" >"$test_dir/stdout.txt" 2>"$test_dir/stderr.txt"
rc=$?
set -e

printf '%s\n' "$rc" >"$test_dir/exitcode.txt"
exit "$rc"
