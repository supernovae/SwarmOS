//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserProfileScopeVerification(t *testing.T) {
	const unit = "swarm-browser-test.scope"
	for _, tc := range []struct {
		name, membership, memory, swap, missing string
		fail                                    bool
	}{
		{name: "verified", membership: "0::/user.slice/" + unit, memory: "2147483648", swap: "0"},
		{name: "wrong membership", membership: "0::/user.slice/other.scope", memory: "2147483648", swap: "0", fail: true},
		{name: "unlimited", membership: "0::/user.slice/" + unit, memory: "max", swap: "0", fail: true},
		{name: "wrong cap", membership: "0::/user.slice/" + unit, memory: "4294967296", swap: "0", fail: true},
		{name: "swap", membership: "0::/user.slice/" + unit, memory: "2147483648", swap: "max", fail: true},
		{name: "unreadable", membership: "0::/user.slice/" + unit, memory: "2147483648", swap: "0", missing: "memory.max", fail: true},
		{name: "v1", membership: "1:memory:/user.slice/" + unit, memory: "2147483648", swap: "0", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if tc.missing != "" && strings.HasSuffix(path, tc.missing) {
					return nil, fmt.Errorf("unreadable")
				}
				switch path {
				case "/proc/self/cgroup":
					return []byte(tc.membership), nil
				case "/sys/fs/cgroup/user.slice/" + unit + "/memory.max":
					return []byte(tc.memory), nil
				case "/sys/fs/cgroup/user.slice/" + unit + "/memory.swap.max":
					return []byte(tc.swap), nil
				default:
					return nil, fmt.Errorf("unexpected read %s", path)
				}
			}
			if err := verifyBrowserScope(unit, read); (err != nil) != tc.fail {
				t.Fatalf("verification: %v", err)
			}
		})
	}
}

func TestBrowserProfileScopeArgs(t *testing.T) {
	args := browserScopeArgs("swarm-browser-test.scope", "/usr/bin/swarm", "/usr/bin/bwrap", []string{"--unshare-all", "--die-with-parent"})
	want := []string{"--user", "--scope", "--quiet", "--unit=swarm-browser-test.scope", "--property=MemoryMax=2147483648", "--property=MemorySwapMax=0", "--", "/usr/bin/swarm", "internal-browser-exec", "swarm-browser-test.scope", "/usr/bin/bwrap", "--unshare-all", "--die-with-parent"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("scope argv: %q", args)
	}
	t.Setenv("GITHUB_TOKEN", "synthetic-secret")
	for _, entry := range browserManagerEnv() {
		if strings.Contains(entry, "synthetic-secret") {
			t.Fatal("credential forwarded")
		}
	}
}

func TestBrowserProfileFlags(t *testing.T) {
	work := t.TempDir()
	original, err := executorFlags(work, false, nil, Dependencies{}, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := executorProfileFlags(work, false, nil, Dependencies{}, "true", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, disabled) {
		t.Fatal("default flags changed")
	}
	enabled, err := executorProfileFlags(work, false, nil, Dependencies{}, "true", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, flag := range original {
		if flag != "--as=8589934592" {
			want = append(want, flag)
		}
	}
	if !reflect.DeepEqual(want, enabled) {
		t.Fatalf("profile changed other flags: %q", enabled)
	}
}

func TestBrowserProfileTransportAndValidationRouting(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode", "omp"} {
		for _, enabled := range []bool{false, true} {
			cfg := &contract.SwarmConfig{DefaultAgent: harness, Agents: map[string]contract.AgentConfig{harness: {Harness: harness}}, Execution: contract.ExecutionConfig{BrowserProfile: enabled}}
			if got := nativeValidationUsesCodex(cfg); got != (harness == "codex" && !enabled) {
				t.Fatalf("%s browser=%v uses Codex=%v", harness, enabled, got)
			}
			args := internalToolArgs(ToolExecutor{Work: "/task", BrowserProfile: enabled})
			if slices.Contains(args, "--browser-profile") != enabled {
				t.Fatalf("startup args %q", args)
			}
		}
	}
}

func TestBrowserProfileScopeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		exit        string
		fail        bool
	}{
		{name: "missing", state: "LoadState=not-found\nActiveState=inactive\nControlGroup=", exit: "1"},
		{name: "empty", state: "LoadState=loaded\nActiveState=inactive\nControlGroup=", exit: "0"},
		{name: "active", state: "LoadState=loaded\nActiveState=active\nControlGroup=", exit: "0", fail: true},
		{name: "wrong group", state: "LoadState=loaded\nActiveState=inactive\nControlGroup=/other.scope", exit: "0", fail: true},
		{name: "manager unavailable", state: "Failed to connect", exit: "1", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\nif [ \"$2\" = stop ]; then exit 0; fi\nprintf '%s\\n' '" + tc.state + "'\nexit " + tc.exit + "\n"
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			if err := stopBrowserScope("swarm-browser-test.scope"); (err != nil) != tc.fail {
				t.Fatalf("cleanup: %v", err)
			}
		})
	}
}

func TestBrowserProfileLive(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for live Bubblewrap/Chromium checks")
	}
	// A prerequisite failure is recorded before running any assertions.
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	if _, err := browserShell(Dependencies{}, ""); err != nil {
		t.Skipf("cache prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	work := t.TempDir()
	fixture, err := os.ReadFile("testdata/browser-preview.sh")
	if err != nil {
		t.Fatal(err)
	}
	var out process.Output
	if err := (ToolExecutor{Work: work, BrowserProfile: true}).Run(t.Context(), string(fixture), &out, &out); err != nil {
		t.Fatalf("preview: %v: %s", err, out.String())
	}
	for _, name := range []string{"dom.html", "preview.png"} {
		if info, err := os.Stat(filepath.Join(work, "browser-output", name)); err != nil || info.Size() == 0 {
			t.Fatalf("missing output %s: %v", name, err)
		}
	}
}

func TestBrowserProfileLiveCancellation(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	work := t.TempDir()
	shell, err := browserShell(Dependencies{}, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cancel", "timeout", "normal"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "timeout" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
				defer deadlineCancel()
			}
			var out process.Output
			done := make(chan error, 1)
			script := `set -eu
setsid sh -c 'sleep 3; echo escaped > delayed' &
python3 -m http.server 8765 --bind 127.0.0.1 > server.log 2>&1 &
"$1" --no-sandbox --headless --disable-gpu --user-data-dir=/tmp/cancel-browser --remote-debugging-port=9222 about:blank > browser.log 2>&1 &
echo started > started
wait`
			if mode == "normal" {
				script = strings.Replace(script, "\nwait", "\nexit 0", 1)
			}
			go func() { done <- (ToolExecutor{Work: work, BrowserProfile: true}).Run(ctx, script, &out, &out, shell) }()
			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(work, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					t.Fatalf("startup not observed: %v", <-done)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "cancel" {
				cancel()
			}
			err := <-done
			if mode != "normal" && err == nil {
				t.Fatal("cancelled command succeeded")
			}
			if mode == "normal" && err != nil && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("normal cleanup: %v: %s", err, out.String())
			}
			time.Sleep(3500 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(work, "delayed")); !os.IsNotExist(err) {
				t.Fatalf("detached descendant escaped: %v", err)
			}
			if err := os.Remove(filepath.Join(work, "started")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBrowserProfileLiveMemory(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd, err := (ToolExecutor{Work: t.TempDir(), BrowserProfile: true}).command(ctx, `sleep 2; python3 -c 'data=bytearray(2300*1024*1024)'; status=$?; sleep 2; exit "$status"`)
	if err != nil {
		t.Fatal(err)
	}
	var unit string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--unit=") {
			unit = strings.TrimPrefix(arg, "--unit=")
			break
		}
	}
	var out process.Output
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stopBrowserScope(unit); err != nil {
			t.Errorf("scope cleanup: %v", err)
		}
		if err := process.StopVerified(cmd); err != nil {
			t.Error(err)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	observedOOM := false
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("allocation above cap succeeded")
			}
			if !observedOOM {
				t.Fatalf("cgroup OOM kill not observed: %v: %s", err, out.String())
			}
			return
		case <-ticker.C:
			show := exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "--property=ControlGroup", "--value")
			show.Env = browserManagerEnv()
			raw, err := show.Output()
			if err != nil {
				continue
			}
			group := strings.TrimSpace(string(raw))
			if !strings.HasPrefix(group, "/") || filepath.Clean(group) != group || filepath.Base(group) != unit {
				continue
			}
			events, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, "memory.events"))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(events), "\n") {
				if strings.HasPrefix(line, "oom_kill ") {
					count, err := strconv.Atoi(strings.TrimPrefix(line, "oom_kill "))
					if err == nil && count > 0 {
						observedOOM = true
					}
				}
			}
		}
	}
}

func TestBrowserProfileToolArgumentsCannotEnable(t *testing.T) {
	e := ToolExecutor{Work: t.TempDir()}
	result := e.Call(t.Context(), "swarm_shell", []byte(`{"command":"true","browser_profile":"true"}`))
	if !result.IsError || result.Content[0].Text != "unexpected tool arguments" {
		t.Fatalf("tool argument accepted: %+v", result)
	}
	if e.BrowserProfile {
		t.Fatal("tool argument changed profile")
	}
}

func TestBrowserProfileLiveHostBoundary(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fmt.Fprint(w, "HOST_BROWSER_SENTINEL") }))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requests.Load() != 1 {
		t.Fatal("host-loopback positive control failed")
	}
	work := t.TempDir()
	shell, err := browserShell(Dependencies{}, work)
	if err != nil {
		t.Fatal(err)
	}
	var out process.Output
	script := `set -eu
python3 - "$2" <<'PY'
import errno, os, socket, sys, urllib.request
assert not os.path.exists('/sys/fs/cgroup/cgroup.controllers')
assert not os.path.exists('/run/user')
assert not any(key in os.environ for key in ('DBUS_SESSION_BUS_ADDRESS','XDG_RUNTIME_DIR','GH_TOKEN','GITHUB_TOKEN','OPENAI_API_KEY'))
try:
    urllib.request.urlopen(sys.argv[1],timeout=1)
except OSError:
    pass
else:
    raise AssertionError('host loopback reachable')
with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as probe:
    try:
        probe.connect(('192.0.2.1',53))
    except OSError as exc:
        assert exc.errno == errno.ENETUNREACH
    else:
        raise AssertionError('outside network route available')
PY
if touch "$PLAYWRIGHT_BROWSERS_PATH/swarm-cache-write-probe" 2>/dev/null; then exit 41; fi
"$1" --no-sandbox --headless --disable-gpu --user-data-dir=/tmp/host-boundary --dump-dom "$2" > host-dom.html 2> host-browser.log
if grep -q HOST_BROWSER_SENTINEL host-dom.html; then exit 42; fi`
	// UDP connect checks host route selection without sending any packets.
	control, err := net.Dial("udp4", "192.0.2.1:53")
	if err != nil {
		t.Skipf("outside-route positive control unavailable: %v", err)
	}
	control.Close()
	if err := (ToolExecutor{Work: work, BrowserProfile: true}).Run(t.Context(), script, &out, &out, shell, server.URL); err != nil {
		t.Fatalf("profile host boundary: %v: %s", err, out.String())
	}
	if requests.Load() != 1 {
		t.Fatal("browser reached the host-loopback canary")
	}
}

func TestBrowserProfileLiveDNSAndOutside(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	positive, err := client.Get("https://example.com/")
	if err != nil {
		t.Skipf("positive host DNS/external HTTPS control unavailable: %v", err)
	}
	body, err := io.ReadAll(io.LimitReader(positive.Body, 1<<20))
	positive.Body.Close()
	if err != nil || !strings.Contains(string(body), "Example Domain") {
		t.Skip("positive host external page control unavailable")
	}
	work := t.TempDir()
	shell, err := browserShell(Dependencies{}, work)
	if err != nil {
		t.Fatal(err)
	}
	var out process.Output
	script := `set -eu
python3 - <<'PY'
import socket
try:
    socket.getaddrinfo('example.com',443)
except OSError:
    pass
else:
    raise AssertionError('public DNS resolved inside the tool')
try:
    socket.create_connection(('1.1.1.1',443),timeout=1)
except OSError:
    pass
else:
    raise AssertionError('outside address reachable')
PY
"$1" --no-sandbox --headless --disable-gpu --user-data-dir=/tmp/dns-boundary --dump-dom https://example.com/ > outside-dom.html 2> outside-browser.log
if grep -q 'Example Domain' outside-dom.html; then exit 42; fi`
	if err := (ToolExecutor{Work: work, BrowserProfile: true}).Run(t.Context(), script, &out, &out, shell); err != nil {
		t.Fatalf("DNS/external boundary: %v: %s", err, out.String())
	}
}

func TestBrowserProfileLiveStartupFailure(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd, unit, err := browserCommand(ctx, "/bin/false", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out process.Output
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err == nil {
		t.Fatal("helper accepted non-Bubblewrap payload")
	}
	if err := process.StopVerified(cmd); err != nil {
		t.Error(err)
	}
	if err := stopBrowserScope(unit); err != nil {
		t.Fatalf("startup failure leaked scope: %v", err)
	}
}

func requireBrowserProfileLive(t *testing.T) {
	t.Helper()
	for _, binary := range []string{"bwrap", "systemd-run", "systemctl", "ldd"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("runtime prerequisite %s: %v", binary, err)
		}
	}
	shell, err := browserShell(Dependencies{}, "")
	if err != nil {
		t.Skipf("cache prerequisite: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	manager := exec.CommandContext(ctx, "systemctl", "--user", "show-environment")
	manager.Env = browserManagerEnv()
	if err := manager.Run(); err != nil {
		t.Skipf("user-manager prerequisite: %v", err)
	}
	libraries, err := exec.CommandContext(ctx, "ldd", shell).CombinedOutput()
	if err != nil || strings.Contains(string(libraries), "not found") {
		t.Skip("Chromium shared-library prerequisite unavailable")
	}
	if err := CheckBrowserProfile(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserProfileScopeEmptiness(t *testing.T) {
	const unit = "swarm-browser-test.scope"
	const state = "LoadState=loaded\nActiveState=inactive\nControlGroup=/user.slice/" + unit + "\n"
	for _, tc := range []struct {
		events  string
		readErr error
		fail    bool
	}{
		{events: "populated 0\nfrozen 0\n"},
		{events: "populated 1\nfrozen 0\n", fail: true},
		{events: "unrecognized\n", fail: true},
		{readErr: os.ErrPermission, fail: true},
		{readErr: os.ErrNotExist},
	} {
		read := func(path string) ([]byte, error) {
			if path == "/sys/fs/cgroup/cgroup.controllers" {
				return []byte("memory"), nil
			}
			if path != "/sys/fs/cgroup/user.slice/"+unit+"/cgroup.events" {
				t.Fatalf("unexpected cleanup read: %s", path)
			}
			return []byte(tc.events), tc.readErr
		}
		if err := verifyBrowserScopeStopped(unit, state, read); (err != nil) != tc.fail {
			t.Fatalf("events=%q: %v", tc.events, err)
		}
	}
}

func TestBrowserProfileScopeRechecksEveryInvocation(t *testing.T) {
	const unit = "swarm-browser-test.scope"
	memory := "2147483648"
	read := func(path string) ([]byte, error) {
		switch filepath.Base(path) {
		case "cgroup":
			return []byte("0::/user.slice/" + unit), nil
		case "memory.max":
			return []byte(memory), nil
		case "memory.swap.max":
			return []byte("0"), nil
		}
		return nil, os.ErrNotExist
	}
	if err := verifyBrowserScope(unit, read); err != nil {
		t.Fatal(err)
	}
	memory = "max"
	if err := verifyBrowserScope(unit, read); err == nil {
		t.Fatal("verification reused a withdrawn memory cap")
	}
}
