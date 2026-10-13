#!/usr/bin/env bash
# CI resource monitor (OPS-001). `resmon.sh <seconds>` prints one line per
# interval: memory, swap, disk, load and the three largest processes by RSS,
# so a runner that runs out of memory leaves evidence in the job log.
# `resmon.sh report` prints the totals and the kernel's OOM records (run it
# after a failure).
set -u
if [ "${1:-}" = report ]; then
	echo "::group::runner resources"
	nproc; free -m; swapon --show; df -h / /mnt 2>/dev/null
	echo "--- kernel OOM records"
	sudo dmesg -T 2>/dev/null | grep -iE 'out of memory|oom-kill|killed process' | tail -20 || true
	journalctl --no-pager -q -u systemd-oomd 2>/dev/null | tail -20 || true
	echo "::endgroup::"
	exit 0
fi
while :; do
	mem=$(free -m | awk '/^Mem:/{printf "mem_used=%dM avail=%dM", $3, $7} /^Swap:/{printf " swap_used=%dM/%dM", $3, $2}')
	disk=$(df -m / | awk 'NR==2{printf "disk_free=%dM", $4}')
	top=$(ps -eo rss=,comm= --sort=-rss | head -3 | awk '{printf "%s=%dM ", $2, $1/1024}')
	echo "[resmon $(date -u +%H:%M:%S)] $mem $disk load=$(cut -d' ' -f1 /proc/loadavg) top: $top"
	sleep "${1:-30}"
done
