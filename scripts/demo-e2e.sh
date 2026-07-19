#!/usr/bin/env bash

set -euo pipefail

repo_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
stub_dir="$repo_dir/testdata/stubtools"
export PATH="$stub_dir:$PATH"

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/cablecheck-demo.XXXXXX")
pc1_pid=
loopback_added=false
loopback_added_with_sudo=false

cleanup() {
	local status=$?
	if [[ -n "$pc1_pid" ]] && kill -0 "$pc1_pid" 2>/dev/null; then
		kill "$pc1_pid" 2>/dev/null || true
		wait "$pc1_pid" 2>/dev/null || true
	fi
	if "$loopback_added"; then
		if "$loopback_added_with_sudo"; then
			sudo -n ip addr del 127.0.0.2/8 dev lo >/dev/null 2>&1 || true
		else
			ip addr del 127.0.0.2/8 dev lo >/dev/null 2>&1 || true
		fi
	fi
	rm -rf -- "$tmpdir"
	exit "$status"
}
trap cleanup EXIT TERM INT

skip() {
	printf 'SKIP: %s\n' "$1"
	printf '%s\n' 'The demo was not run; no failure is reported.'
	exit 0
}

show_logs() {
	printf '%s\n' '--- PC1 output ---'
	if [[ -f "$tmpdir/pc1.log" ]]; then
		cat "$tmpdir/pc1.log"
	fi
	printf '%s\n' '--- PC2 output ---'
	if [[ -f "$tmpdir/pc2.log" ]]; then
		cat "$tmpdir/pc2.log"
	fi
}

fail() {
	printf 'FAIL: %s\n' "$1" >&2
	show_logs >&2
	exit 1
}

if ! lo_json=$(ip -j addr show lo 2>/dev/null); then
	skip "cannot inspect the loopback interface with 'ip -j addr show lo'"
fi

if grep -Eq '"local"[[:space:]]*:[[:space:]]*"127\.0\.0\.2"' <<<"$lo_json"; then
	printf '%s\n' 'Loopback address 127.0.0.2 is already assigned to lo.'
else
	add_error="$tmpdir/loopback-add.err"
	if ip addr add 127.0.0.2/8 dev lo 2>"$add_error"; then
		loopback_added=true
		printf '%s\n' 'Temporarily assigned 127.0.0.2/8 to lo.'
	elif command -v sudo >/dev/null 2>&1 && sudo -n ip addr add 127.0.0.2/8 dev lo 2>"$add_error"; then
		loopback_added=true
		loopback_added_with_sudo=true
		printf '%s\n' 'Temporarily assigned 127.0.0.2/8 to lo with sudo -n.'
	else
		# Linux routes the whole 127/8 block through lo, and CableCheck's
		# --allow-virtual-interface discovery explicitly accepts an address
		# contained by lo's prefix. This is the design's sudo-free demo path.
		if ! grep -Eq '"local"[[:space:]]*:[[:space:]]*"127\.0\.0\.1"[^}]*"prefixlen"[[:space:]]*:[[:space:]]*8' <<<"$lo_json"; then
			skip "127.0.0.2 is not assigned, it could not be added, and lo has no 127.0.0.1/8 assignment"
		fi
		printf '%s\n' 'Loopback alias could not be added without privileges; using the documented 127.0.0.1/8 prefix-contained demo path.'
	fi
fi

if [[ ! -x "$repo_dir/cablecheck" ]]; then
	printf '%s\n' 'Building cablecheck...'
	make -C "$repo_dir" build
fi

mkdir -p "$tmpdir/pc1" "$tmpdir/pc2"

control_port=45200
iperf_port=45201
common_args=(
	run
	--allow-virtual-interface
	--non-interactive
	--no-sudo
	--mode quick
	--token demo-token
	--control-port "$control_port"
	--iperf-port "$iperf_port"
)

"$repo_dir/cablecheck" "${common_args[@]}" \
	--role pc1 \
	--local-ip 127.0.0.1 \
	--peer-ip 127.0.0.2 \
	--output "$tmpdir/pc1" \
	>"$tmpdir/pc1.log" 2>&1 &
pc1_pid=$!

set +e
"$repo_dir/cablecheck" "${common_args[@]}" \
	--role pc2 \
	--local-ip 127.0.0.2 \
	--peer-ip 127.0.0.1 \
	--output "$tmpdir/pc2" \
	>"$tmpdir/pc2.log" 2>&1
pc2_status=$?
wait "$pc1_pid"
pc1_status=$?
set -e
pc1_pid=

# The demo runs over loopback (a virtual interface), so the honest verdict is
# always INCONCLUSIVE (exit 3): the tool refuses to judge a physical cable it
# never touched (rule HOST-02). A completed run therefore exits 3 on both
# sides; exit 0/1/2 would mean a real physical NIC, and 4/5/6/7 are failures
# (config/peer/interrupt/internal) that must not happen here.
readonly expected_exit=3
if (( pc1_status != expected_exit || pc2_status != expected_exit )); then
	fail "peer exit codes were PC1=$pc1_status PC2=$pc2_status; expected both $expected_exit (INCONCLUSIVE for a loopback/virtual interface)"
fi

mapfile -t pc1_reports < <(find "$tmpdir/pc1" -mindepth 1 -maxdepth 1 -type d -name 'cablecheck-report-*' -print)
mapfile -t pc2_reports < <(find "$tmpdir/pc2" -mindepth 1 -maxdepth 1 -type d -name 'cablecheck-report-*' -print)
(( ${#pc1_reports[@]} == 1 )) || fail "expected one PC1 report directory, found ${#pc1_reports[@]}"
(( ${#pc2_reports[@]} == 1 )) || fail "expected one PC2 report directory, found ${#pc2_reports[@]}"
pc1_report=${pc1_reports[0]}
pc2_report=${pc2_reports[0]}

for side in "$pc1_report" "$pc2_report"; do
	for name in report.json report.md summary.txt; do
		[[ -f "$side/$name" ]] || fail "missing report artifact: $side/$name"
	done
done

pc1_sha=$(sha256sum "$pc1_report/report.json" | awk '{print $1}')
pc2_sha=$(sha256sum "$pc2_report/report.json" | awk '{print $1}')
[[ "$pc1_sha" == "$pc2_sha" ]] || fail "report.json hashes differ: PC1=$pc1_sha PC2=$pc2_sha"

if ! "$repo_dir/cablecheck" report "$pc1_report/report.json" >"$tmpdir/report.log" 2>&1; then
	fail 'the report subcommand could not regenerate report.md and summary.txt'
fi

show_logs
cat "$tmpdir/report.log"
printf '%s\n' '--- Demo assertions ---'
printf 'PC1 exit: %s\nPC2 exit: %s  (3 = INCONCLUSIVE, the correct verdict for a loopback interface)\n' "$pc1_status" "$pc2_status"
printf 'PC1 report: %s\n' "$pc1_report"
printf 'PC2 report: %s\n' "$pc2_report"
printf 'report.json sha256: %s (matching)\n' "$pc1_sha"
printf '%s\n' 'report regeneration: OK'
printf '%s\n' 'PASS: CableCheck loopback end-to-end demo completed successfully.'
