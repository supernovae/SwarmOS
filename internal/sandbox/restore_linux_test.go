//go:build linux

package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func buildRestoreHelper(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "restore-helper")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/swarm-restore-helper")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, output)
	}
	return binary
}
func restoreLinuxRequest(t *testing.T) RestoreRequest {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "approved")
	repo := filepath.Join(root, "repository")
	cache := filepath.Join(repo, "private-cache")
	for _, p := range []string{source, cache} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "fixture"), []byte("approved"), 0600); err != nil {
		t.Fatal(err)
	}
	return RestoreRequest{Source: source, RepositoryRoot: repo, CacheRoot: cache, Registries: []string{restoreFixtureHost}, Argv: []string{"/bin/true"}}
}
func requireRestoreCapability(t *testing.T, b RestoreBoundary, r RestoreRequest) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	err := CheckRestoreBoundary(t.Context(), b, r)
	if err == nil {
		return
	}
	// Only these actual infrastructure failures may skip real-launcher tests.
	message := err.Error()
	if strings.Contains(message, "restore Bubblewrap unavailable:") || strings.Contains(message, "restore capability unavailable: Bubblewrap namespaces") ||
		strings.Contains(message, "restore broker unavailable:") && strings.Contains(message, "operation not permitted") ||
		strings.Contains(message, "restore network control unavailable:") && strings.Contains(message, "operation not permitted") {
		t.Skipf("real restore boundary unavailable: %v", err)
	}
	t.Fatalf("restore capability regression: %v", err)
}
func TestRestoreLinuxFlags(t *testing.T) {
	flags, err := restoreFlags("/control", "/private-cache", "127.0.0.1:2345", []string{"/bin/true", "literal;$(no-shell)"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(flags, "\n")
	for _, s := range []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop\nALL", "--clearenv", "--remount-ro\n/", "--ro-bind\n/control/source\n/source", "--bind\n/private-cache\n/cache", "/restore-helper", "literal;$(no-shell)"} {
		if !strings.Contains(joined, s) {
			t.Fatalf("missing isolation flag %q", s)
		}
	}
	for _, s := range []string{"--share-net", "--unshare-user-try", "--preserve-fds", "/bin/sh\n-c", "--bind\n/source", "--ro-bind\n/home", "--ro-bind\n/etc", "--ro-bind\n/control\n"} {
		if strings.Contains(joined, s) {
			t.Fatalf("unsafe flag %q", s)
		}
	}
	cmd, err := restoreCommand(context.Background(), "/control", "/cache", "127.0.0.1:1", nil)
	if err != nil {
		if strings.Contains(err.Error(), "restore Bubblewrap unavailable:") {
			return
		}
		t.Fatal(err)
	}
	if len(cmd.Env) != 1 || cmd.Env[0] != "PATH=/usr/bin:/bin" || len(cmd.ExtraFiles) != 0 {
		t.Fatal("host environment/descriptors inherited")
	}
}
func TestRestoreLinuxHelperStartup(t *testing.T) {
	helper := buildRestoreHelper(t)
	cmd := exec.Command(helper)
	if err := cmd.Run(); err == nil {
		t.Fatal("helper accepted missing control arguments")
	}
}
func TestRestoreLinuxBoundary(t *testing.T) {
	helper := buildRestoreHelper(t)
	b := RestoreBoundary{Helper: helper}
	r := restoreLinuxRequest(t)
	requireRestoreCapability(t, b, r)
	fixture := newRestoreFixture(t, nil)
	b.configureBroker = func(broker *registryBroker) {
		broker.roots = fixture.broker.roots
		broker.resolve = fixture.broker.resolve
		broker.dial = fixture.broker.dial
	}
	t.Setenv("GITHUB_TOKEN", "SYNTHETIC_HOST_TOKEN")
	t.Setenv("HTTPS_PROXY", "http://credential:secret@host.invalid")
	t.Setenv("JAVA_TOOL_OPTIONS", "-Dhost.secret=SYNTHETIC_HOST_TOKEN")
	secret := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(secret, []byte("SYNTHETIC_HOST_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.Open(secret)
	if err != nil {
		t.Fatal(err)
	}
	defer sentinel.Close()
	// Deliberately defeat the usual CLOEXEC default; it still must not cross.
	if _, err = unix.FcntlInt(sentinel.Fd(), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	r.Argv = []string{"/usr/bin/python3", "-c", `
import os, pathlib, socket
assert pathlib.Path('/source/fixture').read_text() == 'approved'
assert 'GITHUB_TOKEN' not in os.environ
assert 'SYNTHETIC_HOST_TOKEN' not in str(os.environ)
assert 'credential:secret' not in str(os.environ)
for p in ['/source/fixture','/outside','/restore-broker/replacement','/restore-trust/ca.pem','/dev/regular']:
 try:
  with open(p,'w') as f: f.write('forbidden')
 except OSError: pass
 else: raise AssertionError(p)
for p in ['/root','/home',os.sys.argv[1],'/proc/1/root'+os.sys.argv[1]]:
 assert not os.path.exists(p), p
for f in pathlib.Path('/proc/self/fd').iterdir():
 try: target=os.readlink(f)
 except OSError: continue
 assert 'host-secret' not in target
for family, target in [(socket.AF_INET,('1.1.1.1',443)),(socket.AF_INET,('169.254.169.254',80)),(socket.AF_INET6,('2606:4700:4700::1111',443))]:
 s=socket.socket(family);s.settimeout(.2)
 try: s.connect(target)
 except OSError: pass
 else: raise AssertionError(target)
 finally: s.close()
assert os.environ['HOME']=='/cache/home'
assert os.environ['TMPDIR']=='/cache/tmp'
assert os.environ['NO_PROXY']==''
assert pathlib.Path('/restore-trust/ca.pem').read_text().startswith('-----BEGIN CERTIFICATE-----')
os.symlink('/source/fixture','/cache/source-escape')
try:
 with open('/cache/source-escape','w') as f: f.write('forbidden')
except OSError: pass
else: raise AssertionError('cache symlink escaped')
import ssl, urllib.parse, http.client
proxy=urllib.parse.urlparse(os.environ['HTTPS_PROXY'])
c=socket.create_connection((proxy.hostname,proxy.port),timeout=3)
c.sendall(b'CONNECT registry.example:443 HTTP/1.1\r\nHost: registry.example:443\r\n\r\n')
header=b''
while not header.endswith(b'\r\n\r\n'): header+=c.recv(1)
assert header.startswith(b'HTTP/1.1 200 ')
s=ssl.create_default_context(cafile=os.environ['SSL_CERT_FILE']).wrap_socket(c,server_hostname='registry.example')
s.sendall(b'GET /artifact HTTP/1.1\r\nHost: registry.example\r\nConnection: close\r\n\r\n')
response=http.client.HTTPResponse(s);response.begin();data=response.read();s.close()
assert data==b'artifact'
pathlib.Path('/cache/result').write_bytes(data)
`, secret}
	output, err := b.Run(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(output)
	data, err := os.ReadFile(filepath.Join(output, "result"))
	if err != nil || string(data) != "artifact" {
		t.Fatal("cache output", err)
	}
	data, err = os.ReadFile(filepath.Join(r.Source, "fixture"))
	if err != nil || string(data) != "approved" {
		t.Fatal("source changed", err)
	}
	data, err = os.ReadFile(secret)
	if err != nil || string(data) != "SYNTHETIC_HOST_TOKEN" {
		t.Fatal("host secret changed", err)
	}
}
func TestRestoreLinuxFailureCleanup(t *testing.T) {
	b := RestoreBoundary{Helper: buildRestoreHelper(t)}
	r := restoreLinuxRequest(t)
	requireRestoreCapability(t, b, r)
	for _, argv := range [][]string{{"/bin/false"}, {"/bin/sh", "-c", "echo partial > /cache/partial; exit 1"}, {"/usr/bin/python3", "-c", `import socket; s=socket.socket(socket.AF_UNIX);s.connect('/restore-broker/broker.sock');s.sendall(b'GET http://evil.example/ HTTP/1.1\r\nHost: evil.example\r\n\r\n');s.close()`}} {
		r.Argv = argv
		if result, err := b.Run(t.Context(), r); err == nil || result != "" {
			t.Fatal("failure reported success")
		}
		entries, err := os.ReadDir(r.CacheRoot)
		if err != nil || len(entries) != 0 {
			t.Fatal("failed output survived", err)
		}
	}
	r.Argv = []string{"/bin/sh", "-c", "setsid /bin/sh -c 'sleep 30; echo escaped > /cache/escaped' & sleep 30"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := b.Run(ctx, r); err == nil {
		t.Fatal("cancellation accepted")
	}
	entries, err := os.ReadDir(r.CacheRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancellation output survived", err)
	}
}
func TestRestoreLinuxCapabilityMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := restoreCommand(t.Context(), "/control", "/cache", "127.0.0.1:1", nil); err == nil || !strings.Contains(err.Error(), "Bubblewrap unavailable") {
		t.Fatal(err)
	}
}
func TestRestoreLinuxSocketProtocol(t *testing.T) {
	// Real TCP-to-Unix bridge, separate from the unconditional in-memory fixtures.
	socket := filepath.Join(t.TempDir(), "broker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("Unix socket infrastructure unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer l.Close()
	trust, err := newRestoreTrust([]string{restoreCanaryHost})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRegistryBroker(t.Context(), l, registryPolicy{}, trust)
	broker.preflight.Store(true)
	broker.serve()
	defer broker.close()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET http://evil.example/ HTTP/1.1\r\nHost: evil.example\r\n\r\n")
	select {
	case <-broker.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("direct Unix socket bypassed inspection")
	}
}

func TestRestoreLinuxSourceSpecialFile(t *testing.T) {
	path := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(path, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := openRestoreDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := openRestoreDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = copyRestoreSource(src, dst, "", new(int64)); err == nil {
		t.Fatal("special file accepted")
	}
}
