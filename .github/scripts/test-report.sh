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
	# A test's own lines (t.Fatal's message among them) are held until it
	# ends: printed in front of its "--- FAIL", dropped when it passes, so
	# `gh run view --log-failed` shows why a test failed (BE-115).
	jq -nRj --unbuffered --arg noise "$noise" --arg alarm "$alarm" '
		foreach (inputs | '"$parse"') as $e ({buf: {}, out: null};
			($e.Package + " " + ($e.Test // "")) as $k
			| if $e.Action == "build-output" or ($e.Action == "output" and $e.Test == null) then
				.out = (if $e.Output | test($noise) then null else $e.Output end)
			elif $e.Action == "output" and ($e.Output | test("^\\s*--- FAIL")) then
				.out = ((.buf[$k] // []) + [$e.Output] | add) | .buf |= del(.[$k])
			elif $e.Action == "output" and ($e.Output | test($alarm)) then .out = $e.Output
			elif $e.Action == "output" and ($e.Output | test($noise) | not) then .buf[$k] += [$e.Output] | .out = null
			elif $e.Action == "pass" or $e.Action == "skip" or $e.Action == "fail" then .buf |= del(.[$k]) | .out = null
			else .out = null end;
			.out | select(. != null))'
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
