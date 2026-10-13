#!/usr/bin/env bash
# Prints `make help` from the "## group|description" annotations on Makefile target lines.
#   build: ## build|Build the binaries
# Groups print in the fixed order below; targets within a group keep Makefile order.
set -euo pipefail

makefile="${1:-Makefile}"

awk '
BEGIN {
	n = split("build free preview paid dev", order, " ")
	title["build"]   = "Build and test (free)"
	title["free"]    = "Free checks (fake providers, no keys, no cost)"
	title["preview"] = "Previews (read-only: show what a paid run would do, spend nothing)"
	title["paid"]    = "PAID live checks (real providers: need GOVOICE_LIVE=1 and examples/.env)"
	title["dev"]     = "Local development"
}
match($0, /^[a-zA-Z0-9_-]+:[^#]*## [a-z]+\|/) {
	name = $0; sub(/:.*/, "", name)
	rest = $0; sub(/^[^#]*## /, "", rest)
	split(rest, parts, "|")
	grp = parts[1]; desc = rest; sub(/^[a-z]+\|/, "", desc)
	lines[grp] = lines[grp] sprintf("  %-24s %s\n", name, desc)
}
END {
	printf "Switchboard make targets  (make <target> [VAR=value])\n"
	for (i = 1; i <= n; i++) {
		g = order[i]
		if (lines[g] != "") printf "\n%s\n%s", title[g], lines[g]
	}
	printf "\nCost tips: paid targets refuse to run without GOVOICE_LIVE=1. Preview first with\n"
	printf "`make live-list`, `make provider-env` and `make agent-matrix-plan`, then run one profile,\n"
	printf "one scenario, one mode (MAX_CALLS / PROVTEST_ARGS=\"-max-calls N\" cap the spend).\n"
	printf "Docs: docs/benchmarks/README.md\n"
}' "$makefile"
