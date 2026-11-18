#!/usr/bin/env bash
# Fails when a gated package's test coverage is below the minimum.
#
# Usage: go test -cover ./... | tee test-output.txt
#        scripts/coverage-gate.sh test-output.txt [minimum, default 90]
#
# The gated packages hold the business rules and the delivery guarantees;
# see docs/milestones.md, M4.
set -euo pipefail

file="${1:?usage: coverage-gate.sh <go test output> [minimum]}"
min="${2:-90}"
gated='internal/(domain|service|worker|api)$'

awk -v min="$min" -v gated="$gated" '
  $1 == "ok" && $2 ~ gated {
    for (i = 1; i <= NF; i++) if ($i == "coverage:") {
      v = $(i + 1); sub("%", "", v)
      status = (v + 0 < min) ? "BELOW " min "%" : "ok"
      printf "%-60s %6s%%  %s\n", $2, v, status
      if (v + 0 < min) fail = 1
      seen++
    }
  }
  END {
    if (seen != 4) { print "expected coverage for 4 gated packages, found " seen + 0; exit 1 }
    exit fail
  }' "$file"
