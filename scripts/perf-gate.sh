#!/bin/sh
# Latency/capacity regression gate (P5-5). Usage: scripts/perf-gate.sh gate|baseline|selftest
# Baselines are per machine class (docs/benchmarks/README.md); never compared across labels.
#   PERF_BASELINE   label override (default: <os>-<arch>-<cpus>c)
#   CI=true         a missing baseline fails, unless PERF_ALLOW_MISSING=1 (then reports are generated, exit 0)
#   FORCE=1         let `baseline` overwrite
#   PERF_SCALE      multiplier on the loadtest tolerance table (default 1)
set -u
cd "$(dirname "$0")/.." || exit 2
mode=${1:-gate}
ncpu=$(sysctl -n hw.ncpu 2>/dev/null || nproc)
label=${PERF_BASELINE:-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)-${ncpu}c}
dir=docs/benchmarks/baselines
out=.perf-out
LB=$dir/$label.json
BB=$dir/$label.bench.json
mkdir -p "$out"

# bench and loadtest exit 1 on regression, 2 on a harness error. A harness error is never
# retried: the last one ("core: session closed", ~4% of runs) was a real core bug.
# Built, not `go run`: go run turns every nonzero exit into 1, hiding harness errors.
go build -o "$out/bin/govoice-bench" ./tools/eval/cmd/govoice-bench && go build -o "$out/bin/loadtest" ./tools/loadtest || exit 2
bench() { "$out/bin/govoice-bench" -sessions 4 -turns 200 "$@"; }
load() {
	[ -f third_party/silero_vad.onnx ] || { echo "perf-gate: third_party/silero_vad.onnx missing" >&2; return 2; }
	"$out/bin/loadtest" -sessions 4 -duration 30s -ramp 3s -turns-per-session 3 -sample-interval 2s \
		-max-p95-v2v 1500 -tolerance-scale "${PERF_SCALE:-1}" "$@"
}

case $mode in
baseline)
	if [ -e "$LB" ] || [ -e "$BB" ]; then
		[ "${FORCE:-}" = 1 ] || { echo "perf-baseline: $LB exists; rerun with FORCE=1 to overwrite (review the diff before committing)" >&2; exit 1; }
	fi
	mkdir -p "$dir"
	bench -json-out "$BB" >/dev/null || exit 1
	load -json-out "$LB" -md-out "$out/loadtest-baseline.md" || exit 1
	echo "perf-baseline: wrote $LB and $BB (label $label). Commit them with a note of the code revision."
	;;
gate)
	if [ ! -f "$LB" ] || [ ! -f "$BB" ]; then
		echo "perf-gate: no baseline for machine label '$label' ($LB, $BB)."
		echo "  Create one on this machine class:  make perf-baseline   (then commit docs/benchmarks/baselines/$label*.json)"
		echo "  Or point at another label:         PERF_BASELINE=<label> make perf-gate"
		if [ "${CI:-}" = true ] && [ "${PERF_ALLOW_MISSING:-}" != 1 ]; then
			echo "perf-gate: FAIL (CI=true, set PERF_ALLOW_MISSING=1 to generate instead)"; exit 1
		fi
		if [ "${CI:-}" = true ]; then # first run on a new runner class: emit the candidate baseline as an artifact
			mkdir -p "$out/baselines"
			bench -json-out "$out/baselines/$label.bench.json" >/dev/null || exit 1
			load -json-out "$out/baselines/$label.json" -md-out "$out/loadtest.md" || exit 1
			echo "perf-gate: generated candidate baseline in $out/baselines (not a pass/fail result)"
		fi
		exit 0
	fi
	rc=0
	# keep the worst exit: 2 (harness error, investigate, not noise) beats 1 (regression)
	echo "=== framework overhead (bench) vs $BB ==="
	bench -json-out "$out/bench.json" -compare "$BB"; r=$?; [ $r -gt $rc ] && rc=$r
	echo "=== loadtest smoke vs $LB ==="
	load -json-out "$out/loadtest.json" -md-out "$out/loadtest.md" -compare "$LB"; r=$?; [ $r -gt $rc ] && rc=$r
	[ -f "$out/loadtest.md" ] && cat "$out/loadtest.md"
	case $rc in
	0) echo "perf-gate: PASS ($label)" ;;
	1) echo "perf-gate: FAIL: regression ($label)" ;;
	*) echo "perf-gate: FAIL: HARNESS ERROR exit $rc ($label): a bug, not a perf result; see stderr above"; rc=2 ;;
	esac
	exit $rc
	;;
selftest)
	[ -f "$LB" ] && [ -f "$BB" ] || { echo "perf-gate-selftest: needs a baseline for '$label' (make perf-baseline)" >&2; exit 2; }
	expect() { # expect <want-exit> <name> cmd...
		want=$1 name=$2; shift 2
		"$@" >"$out/selftest-$name.log" 2>&1; got=$?
		if [ "$got" != "$want" ]; then echo "perf-gate-selftest: FAIL: $name exited $got, wanted $want (see $out/selftest-$name.log)"; exit 1; fi
		echo "perf-gate-selftest: ok: $name exit $got"
	}
	expect 0 bench-clean bench -compare "$BB"
	expect 1 bench-injected bench -inject-delay 100ms -compare "$BB"
	expect 0 load-clean load -compare "$LB"
	( export GOVOICE_FAKE_LLM_DELAY=100ms; expect 1 load-injected load -compare "$LB" ) || exit 1
	# exit 1 alone could be some other row: the injected delay must fail a latency row
	grep -q '^| overhead_p50_ms .*FAIL' "$out/selftest-bench-injected.log" && grep -q '^| v2v_p50_ms .*FAIL' "$out/selftest-load-injected.log" ||
		{ echo "perf-gate-selftest: FAIL: injected delay did not fail a latency row (see $out/selftest-*-injected.log)"; exit 1; }
	echo "perf-gate-selftest: PASS (clean runs pass, 100 ms injected delay fails both gates)"
	;;
*) echo "usage: $0 gate|baseline|selftest" >&2; exit 2 ;;
esac
