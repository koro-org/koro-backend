#!/usr/bin/env bash
# Keeps the Go build caches saved by CI at the size of one run (OPS-017).
#
# Every main push restored the previous cache, added the entries of its own
# commit and saved the lot under a new key, so the caches only grew: the
# security job's cache reached 3 GB compressed and restoring it filled the
# runner's disk ("No space left on device" in the runner's own log, the job
# died inside "Restore Go caches" with no log; runs 41826028360,
# 41826105752, 42030934877).
#
#   go-cache-prune.sh mark DIR...   right after the restore: age every cache
#                                   entry to two hours and drop a marker.
#   go-cache-prune.sh prune DIR...  right before the save: delete the entries
#                                   this run did not use.
#
# Why it works: the go command (and golangci-lint, whose cache is a copy of
# it) touches an entry's mtime when it uses an entry older than one hour, so
# after `mark` every used or new entry is newer than the marker. Two hours
# stays well under the go command's own 5-day trim limit, so its trim never
# deletes a restored entry before this run gets to use it. Entries are the
# `<hash>-a` and `<hash>-d` files; README, trim.txt and the like stay.
# Both modes print the disk and cache sizes, so the job log shows them.
set -u
mode=${1:-}
shift || true
marker="${RUNNER_TEMP:-/tmp}/go-cache-prune.marker"

sizes() {
	df -h / | awk 'NR==2{print "disk: " $4 " free of " $2}'
	for d in "$@"; do [ -d "$d" ] && du -sh "$d"; done
	return 0
}

case "$mode" in
mark)
	ok=1
	for d in "$@"; do
		[ -d "$d" ] || continue
		find "$d" -type f \( -name '*-a' -o -name '*-d' \) -print0 | xargs -0 -r touch -d '2 hours ago' || ok=0
	done
	# Without aged entries a prune would keep everything restored, which is
	# safe; a marker over un-aged entries would delete the ones in use.
	if [ "$ok" = 1 ]; then touch "$marker"; else echo "could not age the entries: no marker, nothing will be pruned"; fi
	sizes "$@"
	;;
prune)
	sizes "$@"
	if [ ! -f "$marker" ]; then
		echo "no marker (mark did not run): nothing pruned"
		exit 0
	fi
	for d in "$@"; do
		[ -d "$d" ] || continue
		n=$(find "$d" -type f \( -name '*-a' -o -name '*-d' \) ! -newer "$marker" -print -delete | wc -l)
		echo "$d: $n unused entries deleted"
	done
	sizes "$@"
	;;
*)
	echo "usage: $0 mark|prune DIR..." >&2
	exit 2
	;;
esac
