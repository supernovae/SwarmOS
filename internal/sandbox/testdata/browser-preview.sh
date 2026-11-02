#!/bin/sh
set -eu
# Run the entire fixture as one swarm_shell command or required check.
: "${PLAYWRIGHT_BROWSERS_PATH:?Provision Playwright Chromium on the host}"
output="${1:-browser-output}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"
state="$(mktemp -d)"
server=
cleanup() {
    if [ -n "$server" ]; then kill "$server" 2>/dev/null || :; wait "$server" 2>/dev/null || :; fi
    rm -rf "$state"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
shell=
for candidate in "$PLAYWRIGHT_BROWSERS_PATH"/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell; do
    if [ -x "$candidate" ]; then shell="$candidate"; break; fi
done
test -n "$shell"
mkdir "$state/page"
printf '%s\n' '<!doctype html><title>Preview</title><p id="result">waiting</p><script>document.getElementById("result").textContent="SWARM_BROWSER_OK"</script>' > "$state/page/index.html"
python3 -m http.server 8765 --bind 127.0.0.1 --directory "$state/page" > "$output/server.log" 2>&1 &
server=$!
printf '%s\n' 8765 > "$state/port"
python3 - "$state/port" <<'PY'
import pathlib, sys, time, urllib.request
portfile = pathlib.Path(sys.argv[1])
for _ in range(100):
    try:
        port = int(portfile.read_text())
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=.2) as response:
            assert response.status == 200
        break
    except (OSError, ValueError):
        time.sleep(.05)
else:
    raise SystemExit("preview server did not become ready")
PY
url="http://127.0.0.1:$(cat "$state/port")/"
"$shell" --no-sandbox --headless --disable-gpu --user-data-dir="$state/dom" --dump-dom "$url" > "$output/dom.html" 2> "$output/dom.log"
"$shell" --no-sandbox --headless --disable-gpu --user-data-dir="$state/png" --screenshot="$output/preview.png" "$url" > "$output/screenshot.log" 2>&1
python3 - "$output" <<'PY'
import pathlib, sys
out = pathlib.Path(sys.argv[1])
assert '<p id="result">SWARM_BROWSER_OK</p>' in (out / 'dom.html').read_text()
assert (out / 'preview.png').read_bytes().startswith(b'\x89PNG\r\n\x1a\n')
PY
