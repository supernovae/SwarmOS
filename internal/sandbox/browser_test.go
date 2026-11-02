//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/storageuse"
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
	"syscall"
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
	want := []string{"--user", "--scope", "--expand-environment=no", "--quiet", "--unit=swarm-browser-test.scope", "--property=MemoryMax=2147483648", "--property=MemorySwapMax=0", "--", "/usr/bin/swarm", "internal-browser-exec", "swarm-browser-test.scope", "/usr/bin/bwrap", "--unshare-all", "--die-with-parent"}
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
browser_status=0
"$1" --no-sandbox --headless --disable-gpu --user-data-dir=/tmp/host-boundary --dump-dom "$2" > host-dom.html 2> host-browser.log || browser_status=$?
test "$browser_status" -eq 0 || test "$browser_status" -eq 1
grep -q "net::ERR_" host-browser.log
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
browser_status=0
"$1" --no-sandbox --headless --disable-gpu --user-data-dir=/tmp/dns-boundary --dump-dom https://example.com/ > outside-dom.html 2> outside-browser.log || browser_status=$?
test "$browser_status" -eq 0 || test "$browser_status" -eq 1
grep -q "net::ERR_" outside-browser.log
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

func TestBrowserProfileLauncherCapability(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fail       bool
	}{
		{"supported", "printf '%s\\n' '--expand-environment=BOOL'", false},
		{"old", "printf '%s\\n' 'systemd-run options'", true},
		{"failed probe", "exit 1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launcher := filepath.Join(t.TempDir(), "systemd-run")
			if err := os.WriteFile(launcher, []byte("#!/bin/sh\n"+tc.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := checkBrowserLauncher(t.Context(), launcher); (err != nil) != tc.fail {
				t.Fatalf("capability: %v", err)
			}
		})
	}
}

func TestBrowserProfileLiveLiteralEnvironment(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if err := browserScopePrerequisites(); err != nil {
		t.Skipf("runtime prerequisite: %v", err)
	}
	requireBrowserProfileLive(t)
	var out process.Output
	script := `set -eu
printf '%s\n' '${VAR}' '${GH_TOKEN+x}'
VAR=sandbox-value
test "${VAR}" = sandbox-value
test -z "${GH_TOKEN+x}"`
	if err := (ToolExecutor{Work: t.TempDir(), BrowserProfile: true}).Run(t.Context(), script, &out, &out); err != nil {
		t.Fatalf("literal payload: %v: %s", err, out.String())
	}
	if out.String() != "${VAR}\n${GH_TOKEN+x}\n" {
		t.Fatalf("payload expanded before sandbox shell: %q", out.String())
	}
}

// This fixture replaces only the kernel-membership read in the helper. It
// exercises the production runner's private pipes without certifying a cgroup.
func TestNativeControlFixture(t *testing.T) {
	if os.Getenv("SWARM_NATIVE_CONTROL_FIXTURE") != "1" {
		return
	}
	payload := os.NewFile(3, "payload")
	identity := os.NewFile(4, "identity")
	ack := os.NewFile(5, "ack")
	var p nativePayload
	if err := readNativeFrame(payload, &p); err != nil {
		os.Exit(71)
	}
	payload.Close()
	n := nativeFixtureIdentity()
	if err := writeNativeFrame(identity, n); err != nil {
		os.Exit(72)
	}
	identity.Close()
	var approved [1]byte
	if _, err := io.ReadFull(ack, approved[:]); err != nil || approved[0] != 1 {
		os.Exit(73)
	}
	ack.Close()
	if err := os.Chdir(p.Dir); err != nil {
		os.Exit(74)
	}
	if err := syscall.Exec(p.Binary, append([]string{p.Arg0}, p.Args...), p.Env); err != nil {
		os.Exit(75)
	}
}

func nativeFixtureIdentity() storageuse.NativeIdentity {
	return storageuse.NativeIdentity{Boot: "11111111-1111-1111-1111-111111111111", Unit: "swarm-native-0123456789abcdef0123456789abcdef.scope", Group: "/user.slice/swarm-native-0123456789abcdef0123456789abcdef.scope", MountDevice: 1, MountInode: 2, Inode: 3}
}

type nativeFixtureBoundary struct {
	verifyErr, stopErr      error
	stopped                 bool
	commandEnv, commandArgs []string
}

func (n *nativeFixtureBoundary) command(ctx context.Context, _, dir string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeControlFixture$")
	cmd.Dir, cmd.Env = dir, []string{"SWARM_NATIVE_CONTROL_FIXTURE=1"}
	n.commandEnv, n.commandArgs = cmd.Env, cmd.Args
	return cmd, nil
}
func (n *nativeFixtureBoundary) verify(storageuse.NativeIdentity) error { return n.verifyErr }
func (n *nativeFixtureBoundary) stop(storageuse.NativeIdentity) error {
	n.stopped = true
	return n.stopErr
}

type nativePayloadObservation struct {
	Args, Env      []string
	Dir, Input     string
	ControlsClosed bool
}

func TestNativePayloadFixture(t *testing.T) {
	if os.Getenv("SWARM_NATIVE_PAYLOAD_FIXTURE") != "1" {
		return
	}
	if os.Getenv("SWARM_NATIVE_FIXTURE_WAIT") == "1" {
		fmt.Fprintln(os.Stdout, "ready")
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		os.Exit(0)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(81)
	}
	dir, err := os.Getwd()
	if err != nil {
		os.Exit(82)
	}
	controlsClosed := true
	for fd := 3; fd <= 5; fd++ {
		// Go may reuse closed descriptors during runtime initialization. The
		// inherited pipe names must be absent even if a runtime descriptor exists.
		target, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		if strings.HasPrefix(target, "pipe:") {
			controlsClosed = false
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(nativePayloadObservation{Args: os.Args, Env: os.Environ(), Dir: dir, Input: string(input), ControlsClosed: controlsClosed})
	os.Exit(0)
}

func TestNativeScopeLiteralEnvironmentArgument(t *testing.T) {
	env := []string{"SWARM_NATIVE_PAYLOAD_FIXTURE=1", "VAR=expanded-sentinel", "PATH=/usr/bin:/bin", "SYNTHETIC_TOKEN=private-payload-value"}
	args := []string{"-test.run=^TestNativePayloadFixture$", "--", "${VAR}"}
	boundary := &nativeFixtureBoundary{}
	var out, diagnostics process.Output
	dir := resolvedTempDir(t)
	err := runNativeCommandWithBoundary(context.Background(), os.Args[0], dir, env, args, strings.NewReader("stdin bytes"), &out, &diagnostics, process.StopVerified, func(context.Context) (nativeScopeBoundary, error) { return boundary, nil })
	if err != nil {
		t.Fatalf("native transport: %v: %s", err, diagnostics.String())
	}
	var got nativePayloadObservation
	if err = json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, append([]string{os.Args[0]}, args...)) || !reflect.DeepEqual(got.Env, env) || got.Dir != dir || got.Input != "stdin bytes" || !got.ControlsClosed {
		t.Fatalf("payload transport: %+v", got)
	}
	if !boundary.stopped {
		t.Fatal("boundary teardown omitted")
	}
	for _, arg := range append(boundary.commandEnv, boundary.commandArgs...) {
		if strings.Contains(arg, "private-payload-value") {
			t.Fatal("payload environment escaped private pipe")
		}
	}
	launch := nativeScopeArgs(nativeFixtureIdentity().Unit, "/usr/bin/swarm")
	if !slices.Contains(launch, "--expand-environment=no") {
		t.Fatal("scope expansion enabled")
	}
	for _, arg := range launch {
		if strings.Contains(arg, "Memory") || strings.Contains(arg, "Delegate") {
			t.Fatal("native scope inherited browser policy")
		}
	}
}

func TestNativeScopeFailureRetainsUse(t *testing.T) {
	for _, mode := range []string{"verify", "stop", "process stop", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			isolateRuntimeControl(t)
			ctx, scope, err := storageuse.Ensure(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			dir, err := scope.TempDir(ctx, "", "swarm-native-failure-*", "native-scratch")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			failure := errors.New("injected boundary failure")
			boundary := &nativeFixtureBoundary{stopErr: failure}
			if mode == "verify" {
				boundary.verifyErr = failure
			}
			stop := process.StopVerified
			if mode == "process stop" {
				boundary.stopErr = nil
				stop = func(cmd *exec.Cmd) error { return errors.Join(process.StopVerified(cmd), failure) }
			}
			args := []string{"-test.run=^TestNativePayloadFixture$"}
			env := []string{"SWARM_NATIVE_PAYLOAD_FIXTURE=1"}
			var input io.Reader
			var out io.Writer = io.Discard
			if mode == "cancellation" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				reader, writer, pipeErr := os.Pipe()
				if pipeErr != nil {
					t.Fatal(pipeErr)
				}
				defer reader.Close()
				defer writer.Close()
				input = reader
				output, ready, pipeErr := os.Pipe()
				if pipeErr != nil {
					t.Fatal(pipeErr)
				}
				defer output.Close()
				defer ready.Close()
				out = ready
				env = append(env, "SWARM_NATIVE_FIXTURE_WAIT=1")
				if err = output.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				go func() { _, _ = bufio.NewReader(output).ReadString('\n'); cancel() }()
			}
			err = runNativeCommandWithBoundary(ctx, os.Args[0], dir, env, args, input, out, io.Discard, stop, func(context.Context) (nativeScopeBoundary, error) { return boundary, nil })
			if !errors.Is(err, failure) || !boundary.stopped || !scope.Retained() {
				t.Fatalf("uncertain teardown: stopped=%v retained=%v err=%v", boundary.stopped, scope.Retained(), err)
			}
			if err = scope.Finish(); err != nil {
				t.Fatal(err)
			}
			assertRuntimePending(t, 1)
		})
	}
}

func TestNativeScopeUnavailablePreservesCommand(t *testing.T) {
	for _, reason := range []string{"no cgroup v2", "missing launcher", "missing manager", "unreachable manager", "unsupported expansion"} {
		t.Run(reason, func(t *testing.T) {
			isolateRuntimeControl(t)
			ctx, scope, err := storageuse.Ensure(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			dir, err := scope.TempDir(ctx, "", "swarm-native-legacy-*", "native-scratch")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			if err = scope.AcquirePrepared(ctx, storageuse.ResourceFor(scope.Repository(), "agent-build", dir+"-cache")); err != nil {
				t.Fatal(err)
			}
			var out, diagnostics process.Output
			env := []string{"SWARM_NATIVE_PAYLOAD_FIXTURE=1", "VAR=expanded-sentinel"}
			err = runNativeCommandWithBoundary(ctx, os.Args[0], dir, env, []string{"-test.run=^TestNativePayloadFixture$", "--", "${VAR}"}, strings.NewReader("legacy input"), &out, &diagnostics, process.StopVerified, func(context.Context) (nativeScopeBoundary, error) {
				return nil, fmt.Errorf("%s: %w", reason, errNativeScopeUnavailable)
			})
			if err != nil {
				t.Fatalf("unavailable: %v %s", err, diagnostics.String())
			}
			var got nativePayloadObservation
			if err = json.Unmarshal([]byte(out.String()), &got); err != nil {
				t.Fatal(err)
			}
			if got.Args[len(got.Args)-1] != "${VAR}" || !reflect.DeepEqual(got.Env, env) || got.Dir != dir || got.Input != "legacy input" {
				t.Fatalf("legacy transport %+v", got)
			}
			scope.Retain()
			if err = scope.Finish(); err != nil {
				t.Fatal(err)
			}
			assertRuntimePending(t, 2)
			root, _ := storageuse.ControlRoot()
			files, _ := filepath.Glob(filepath.Join(root, "*.json"))
			outside := false
			for _, path := range files {
				raw, _ := os.ReadFile(path)
				outside = outside || strings.Contains(string(raw), `"Outside":true`)
			}
			if !outside {
				t.Fatal("legacy launch lacks permanent Outside evidence")
			}
		})
	}
}

func init() {
	nativeTestDispatch = func() {
		if os.Getenv("SWARM_NATIVE_CONTROL_FIXTURE") == "1" {
			TestNativeControlFixture(nil)
			os.Exit(90)
		}
		if os.Getenv("SWARM_NATIVE_PAYLOAD_FIXTURE") == "1" {
			TestNativePayloadFixture(nil)
			os.Exit(91)
		}
	}
}

// Runs automatically when the host exposes cgroup v2 and a usable user manager.
// The test executable dispatches the same hidden helper as the installed CLI.
func TestNativeScopeRealContainment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prepared, err := prepareNativeScope(ctx)
	if errors.Is(err, errNativeScopeUnavailable) {
		t.Skip("real native scope requires mounted cgroup v2, systemd-run with --expand-environment=no, and a reachable user manager")
	}
	if err != nil {
		t.Fatal(err)
	}
	isolateRuntimeControl(t)
	ctx, owner, err := storageuse.Ensure(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Finish()
	dir, err := owner.TempDir(ctx, "", "swarm-real-native-*", "native-scratch")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err = owner.AcquirePrepared(ctx, storageuse.ResourceFor(owner.Repository(), "agent-build", dir+"-cache")); err != nil {
		t.Fatal(err)
	}
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer output.Close()
	if err = reader.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "delayed-write")
	env := []string{"SWARM_COMPLETION_FIXTURE_ROLE=parent", "SWARM_COMPLETION_FIXTURE_OUTPUT=" + path}
	err = runNativeCommandWithBoundary(ctx, os.Args[0], dir, env, []string{"-test.run=^TestNativeCommandSuccessCanLeaveStorageConsumer$"}, input, output, output, process.StopVerified, func(context.Context) (nativeScopeBoundary, error) { return prepared, nil })
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// The readiness pipe guarantees the detached child existed before teardown.
	// A delayed write must now be impossible, even if the parent exited cleanly.
	input.Close()
	if _, err = writer.Write([]byte{'x'}); err == nil {
		t.Fatal("detached descendant still holds stdin after scope teardown")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("detached descendant wrote after teardown: %v", err)
	}
	root, _ := storageuse.ControlRoot()
	files, _ := filepath.Glob(filepath.Join(root, "*.json"))
	confirmed := false
	for _, file := range files {
		raw, _ := os.ReadFile(file)
		confirmed = confirmed || strings.Contains(string(raw), `"Empty":true`)
	}
	if !confirmed {
		t.Fatal("durable native empty evidence missing")
	}
	// Repeat literal argv transport through the actual systemd launcher.
	actual, err := prepareNativeScope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out process.Output
	err = runNativeCommandWithBoundary(ctx, os.Args[0], dir, []string{"SWARM_NATIVE_PAYLOAD_FIXTURE=1", "VAR=expanded-sentinel"}, []string{"-test.run=^TestNativePayloadFixture$", "--", "${VAR}"}, nil, &out, &out, process.StopVerified, func(context.Context) (nativeScopeBoundary, error) { return actual, nil })
	if err != nil {
		t.Fatal(err)
	}
	var got nativePayloadObservation
	if err = json.Unmarshal([]byte(out.String()), &got); err != nil || len(got.Args) == 0 || got.Args[len(got.Args)-1] != "${VAR}" {
		t.Fatalf("real literal transport: %s: %v", out.String(), err)
	}
}

func TestNativeScopeCapabilitySelection(t *testing.T) {
	for _, mode := range []string{"available", "cgroup absent", "launcher absent", "manager absent", "expansion unsupported", "manager unreachable"} {
		t.Run(mode, func(t *testing.T) {
			checks := nativeScopeCapabilities{
				stat: func(string) (os.FileInfo, error) {
					if mode == "cgroup absent" {
						return nil, os.ErrNotExist
					}
					return nil, nil
				},
				lookup: func(name string) (string, error) {
					if name == "systemd-run" && mode == "launcher absent" || name == "systemctl" && mode == "manager absent" {
						return "", exec.ErrNotFound
					}
					return "/usr/bin/" + name, nil
				},
				launcher: func(context.Context, string) error {
					if mode == "expansion unsupported" {
						return errors.New("unknown expansion option")
					}
					return nil
				},
				manager: func(context.Context, string) error {
					if mode == "manager unreachable" {
						return errors.New("no user bus")
					}
					return nil
				},
			}
			boundary, err := prepareNativeScopeWith(context.Background(), checks)
			if mode == "available" {
				if err != nil || boundary == nil {
					t.Fatalf("available: %v", err)
				}
			} else if !errors.Is(err, errNativeScopeUnavailable) || boundary != nil {
				t.Fatalf("unavailable: %v %v", boundary, err)
			}
		})
	}
}

func TestNativeScopeFrameValidation(t *testing.T) {
	for _, data := range [][]byte{{0, 0, 0, 0}, {255, 255, 255, 255}, {0, 0, 0, 8, '{', '}'}} {
		if err := readNativeFrame(bytes.NewReader(data), new(nativePayload)); err == nil {
			t.Fatal("accepted invalid native control frame")
		}
	}
	var encoded bytes.Buffer
	if err := writeNativeFrame(&encoded, map[string]string{"unknown": "value"}); err != nil {
		t.Fatal(err)
	}
	if err := readNativeFrame(&encoded, new(nativePayload)); err == nil {
		t.Fatal("accepted unknown control field")
	}
	if err := writeNativeFrame(io.Discard, strings.Repeat("x", nativeFrameLimit)); err == nil {
		t.Fatal("accepted oversized native control frame")
	}
}

func TestNativeScopeUnavailableExitAndCancellation(t *testing.T) {
	unavailable := func(context.Context) (nativeScopeBoundary, error) { return nil, errNativeScopeUnavailable }
	var out process.Output
	err := runNativeCommandWithBoundary(context.Background(), "/bin/sh", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, []string{"-c", "printf exactly-once; exit 27"}, nil, &out, &out, process.StopVerified, unavailable)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 27 || out.String() != "exactly-once" {
		t.Fatalf("legacy exit: %v %q", err, out.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err = reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(reader).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected readiness %q", line)
		}
		ready <- err
		cancel()
	}()
	err = runNativeCommandWithBoundary(ctx, "/bin/sh", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, []string{"-c", "printf 'ready\\n'; exec sleep 60"}, nil, writer, io.Discard, process.StopVerified, unavailable)
	if readyErr := <-ready; readyErr != nil {
		t.Fatal(readyErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("legacy cancellation: %v", err)
	}
}

func TestNativeScopeStopAfterUnitUnloaded(t *testing.T) {
	for _, remainsPopulated := range []bool{false, true} {
		t.Run(fmt.Sprintf("remains-populated=%t", remainsPopulated), func(t *testing.T) {
			scope := &nativeSystemdScope{unit: "swarm-native-test.scope"}
			identity := storageuse.NativeIdentity{Unit: scope.unit}
			populated := true
			checks := 0
			var calls [][]string
			cmd := exec.Command("/bin/sh", "-c", "exit 5")
			unitNotLoaded := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(unitNotLoaded, &exitErr) || exitErr.ExitCode() != 5 {
				t.Fatalf("expected injected exit status 5, got %v", unitNotLoaded)
			}
			err := scope.stopWith(identity, func(got storageuse.NativeIdentity) bool {
				checks++
				if got != identity {
					t.Fatalf("checked different boundary: %+v", got)
				}
				return !populated
			}, func(args ...string) error {
				calls = append(calls, args)
				if args[0] == "kill" {
					populated = remainsPopulated
					return nil
				}
				return unitNotLoaded
			})
			want := [][]string{{"kill", "--kill-whom=all", "--signal=KILL", scope.unit}, {"stop", scope.unit}}
			if checks != 2 || !reflect.DeepEqual(calls, want) {
				t.Fatalf("checks=%d calls=%v", checks, calls)
			}
			if remainsPopulated {
				if !errors.Is(err, storageuse.ErrUnsafe) || !errors.Is(err, unitNotLoaded) {
					t.Fatalf("expected unsafe boundary and systemctl error, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("verified empty boundary must stop cleanly: %v", err)
			}
		})
	}
}
