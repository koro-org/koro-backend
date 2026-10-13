#!/usr/bin/env bash
# Readable CI output for `go test -json` (OPS-001). The JSON stream carries
# every test's log, so the measured durations (and the smoke test's flow
# time) are visible without -v; the log stays short when tests pass.
#   make test-integration | tee test.json | test-report.sh live
#       prints package results, build errors, failures and the smoke time
#       (testcontainers' start-up chatter is left out; `failures` has it)
#   test-report.sh slowest test.json   slowest packages and top-level tests
#   test-report.sh failures test.json  full output of every failed test
# Lines that are not JSON (make's own output) are printed as they are.
set -euo pipefail
parse='(fromjson? // {Action: "output", Output: (. + "\n")})'
# Test output that is printed live: failures, panics (a -timeout panic is
# attributed to the running tests, which then never report "fail"), races.
alarm='^\s*--- FAIL|^panic:|test timed out|WARNING: DATA RACE|race detected|smoke flow took'
noise='^([0-9]{4}/[0-9]{2}/[0-9]{2} [0-9:]{8} |  (Server Version|API Version|Operating System|Total Memory|Testcontainers for Go Version|Resolved Docker Host|Resolved Docker Socket Path|Test SessionID|Test ProcessID): )'
case "${1:-}" in
live)
	jq -Rj --unbuffered "$parse"' | select(.Action == "build-output"
		or (.Action == "output" and (.Test == null or (.Output | test($alarm)))))
		| select(.Output | test($noise) | not) | .Output' --arg noise "$noise" --arg alarm "$alarm"
	;;
slowest)
	echo "--- slowest packages (s)"
	jq -Rr "$parse"' | select((.Action == "pass" or .Action == "fail") and .Test == null and .Elapsed != null)
		| "\(.Elapsed)\t\(.Package)"' "$2" | sort -rn | awk 'NR <= 10'
	echo "--- slowest top-level tests (s)"
	jq -Rr "$parse"' | select((.Action == "pass" or .Action == "fail") and .Test != null and (.Test | contains("/") | not))
		| "\(.Elapsed)\t\(.Package) \(.Test)"' "$2" | sort -rn | awk 'NR <= 15'
	;;
failures)
	# Every failed test's output, and the package-level output (TestMain,
	# containers, panics) of every failed package.
	# A test that started but never ended (the binary panicked or timed out)
	# counts as failed too.
	failed=$(jq -Rc "$parse"' | select(.Action == "run" or .Action == "pass" or .Action == "skip" or .Action == "fail")
		| [.Package + " " + (.Test // ""), .Action]' "$2" |
		jq -sc 'group_by(.[0]) | map(select(map(.[1]) | (index("fail") != null) or (index("pass") == null and index("skip") == null)) | .[0][0])')
	jq -Rj --argjson f "$failed" "$parse"' | select(.Action == "output")
		| select((.Package + " " + (.Test // "")) as $k | $f | index([$k]) != null) | .Output' "$2"
	;;
*)
	echo "usage: $0 live | slowest <file> | failures <file>" >&2
	exit 2
	;;
esac
