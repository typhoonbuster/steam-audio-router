#!/bin/bash
# End-to-end test on the real PipeWire daemon, without touching your own nodes.
#
# It creates two temporary things, both silent and both wrapped in `timeout`
# so nothing can outlive the test:
#   - a virtual sink "input.sar-test-recording" (pw-loopback whose output side
#     has node.autoconnect=false, so it plays nowhere);
#   - a silent stream "output.sar-test-loop" started under a parent process
#     whose comm is "steam" (a symlink to bash), so the router sees a Steam
#     descendant.
# Then it runs the router in REAL mode with a config that targets the test
# sink in all-steam-children mode, and checks that exactly two links appear
# (FL and FR), that nothing else is linked, and that everything vanishes on
# teardown.
#
# Usage: tests/integration.sh [path/to/steam-audio-router]
set -u
BIN="${1:-$(dirname "$0")/../release/steam-audio-router}"
BIN="$(readlink -f "$BIN")"
[[ -x "$BIN" ]] || { echo "binary not found: $BIN (run make build first)"; exit 2; }
T="$(mktemp -d)"
ln -s /bin/bash "$T/steam"
cat > "$T/config.toml" <<CFG
target_node_patterns = ["sar-test-recording"]
steam_ancestor_comms = ["steam"]
selection_mode = "all-steam-children"
log_level = "info"
link_verify_delay = 1.0
CFG
PIDS=()
cleanup() { kill "${PIDS[@]}" 2>/dev/null; wait 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT

echo "=== 1. temporary sink (self-destructs after 25 s) ==="
timeout 25 pw-loopback --name sar-test-recording -i '{ media.class=Audio/Sink audio.position=[FL FR] }' -o '{ node.autoconnect=false }' >/dev/null 2>&1 &
PIDS+=($!)
sleep 1
echo "=== 2. silent stream under a parent whose comm is 'steam' (20 s) ==="
"$T/steam" -c "timeout 20 pw-loopback --name sar-test-loop -i '{ node.autoconnect=false }' -o '{ node.autoconnect=false }'" >/dev/null 2>&1 &
FAKE=$!; PIDS+=($FAKE)
sleep 1.5
echo "=== 3. router in REAL mode, targeting the test sink only (15 s) ==="
timeout -s TERM 15 "$BIN" run --config "$T/config.toml" > "$T/router.log" 2>&1 &
ROUTER=$!; PIDS+=($ROUTER)
sleep 5
echo "--- pw-link -l: cables arriving on the test sink ---"
pw-link -l 2>/dev/null | grep -B1 "sar-test-recording:playback" | grep -v "^--"
LINKS=$(pw-link -l 2>/dev/null | grep -c "sar-test-recording:playback")
echo "=== 4. stream quits -> links die; then the sink quits; then SIGTERM ==="
kill "$FAKE" $(pgrep -P "$FAKE") 2>/dev/null; sleep 2
kill "${PIDS[0]}" 2>/dev/null; sleep 2
kill -TERM "$ROUTER" 2>/dev/null; wait "$ROUTER" 2>/dev/null
echo "=== router log ==="; cut -c1-160 "$T/router.log"
LEFT=$(pw-dump -N 2>/dev/null | grep -c "sar-test")
echo "=== result ==="
echo "links created: $LINKS (expected 2) ; leftovers: $LEFT (expected 0)"
if [[ "$LINKS" == 2 && "$LEFT" == 0 ]] && grep -q "Links effective" "$T/router.log"; then
	echo "PASS"
else
	echo "FAIL"; exit 1
fi
