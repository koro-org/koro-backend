#!/usr/bin/env bash
# Downloads a pinned release file, verifies its SHA-256 and keeps a copy in
# a content-addressed cache (OPS-019).
#
#   fetch-tool.sh URL SHA256 DEST
#
# GitHub release downloads answered 504 for minutes at a time on 2026-11-05
# (Opengrep, gitleaks, Syft; runs 42045478926, 42046137662, 42053879312).
# `curl --retry 3` gave up within seven seconds, so:
#   - the cache ($KORO_TOOL_CACHE, default ~/.cache/koro-release-tools, kept
#     between runs with actions/cache) holds every file under its SHA-256;
#     a cached copy is verified again before use, a bad one is downloaded
#     again;
#   - a download is tried up to 6 times over about two minutes, each time
#     into a fresh file, on any curl error (504 included);
#   - a file that arrives but does not match the SHA-256 fails at once,
#     without retrying: that is not an outage.
set -euo pipefail
url=${1:?url}
sha=${2:?sha256}
dest=${3:?dest}
cache=${KORO_TOOL_CACHE:-$HOME/.cache/koro-release-tools}
mkdir -p "$cache" "$(dirname "$dest")"
cached="$cache/$sha"

ok() { echo "$sha  $1" | sha256sum -c --status -; }

if [ -f "$cached" ] && ok "$cached"; then
	echo "fetch-tool: cache hit ${url##*/} ($sha)"
	cp "$cached" "$dest"
	exit 0
fi
rm -f "$cached"

tmp="$cached.part"
for delay in 5 10 20 30 40 0; do
	rm -f "$tmp"
	if curl -fsSL --connect-timeout 20 --max-time 300 -o "$tmp" "$url"; then
		if ! ok "$tmp"; then
			echo "::error::fetch-tool: SHA-256 mismatch for $url (want $sha, got $(sha256sum "$tmp" | cut -d' ' -f1))"
			rm -f "$tmp"
			exit 1
		fi
		mv "$tmp" "$cached"
		cp "$cached" "$dest"
		echo "fetch-tool: downloaded ${url##*/} ($sha)"
		exit 0
	fi
	[ "$delay" -gt 0 ] || break
	echo "fetch-tool: download of $url failed; retrying in ${delay}s"
	sleep "$delay"
done
rm -f "$tmp"
echo "::error::fetch-tool: could not download $url (6 attempts)"
exit 1
