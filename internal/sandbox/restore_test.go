package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRestorePolicyAuthorities(t *testing.T) {
	policy, err := newRegistryPolicy([]string{restoreFixtureHost})
	if err != nil {
		t.Fatal(err)
	}
	if host, err := parseRegistryTarget("registry.example:443", policy); err != nil || host != restoreFixtureHost {
		t.Fatal(host, err)
	}
	for _, authority := range []string{"cdn.example:443", "registry.example.evil:443", "evilregistry.example:443", "*.registry.example:443", "REGISTRY.example:443", "registry.example.:443", "registry.example:0443", "registry.example:80", "registry.example", "registry.example:", "registry.example:443:443", "u:p@registry.example:443", "registry.example%00:443", "%72egistry.example:443", "registry.example\\evil:443", "registry.example :443", "registry.example\n:443", "https://registry.example:443", "127.0.0.1:443", "[::1]:443", "2130706433:443"} {
		t.Run(authority, func(t *testing.T) {
			if _, err := parseRegistryTarget(authority, policy); err == nil {
				t.Fatal("unsafe authority accepted")
			}
		})
	}
	for _, host := range []string{"*.example", "registry.example.", "REGISTRY.example", "127.0.0.1", "[::1]", "localhost", restoreCanaryHost, "x..example", "-x.example", "x-.example", "x.123", "x_foo.example"} {
		if _, err := newRegistryPolicy([]string{host}); err == nil {
			t.Fatalf("unsafe policy accepted %q", host)
		}
	}
	if _, err := newRegistryPolicy(nil); err == nil {
		t.Fatal("empty policy")
	}
}
func TestRestoreAddressSafety(t *testing.T) {
	for _, s := range []string{"0.0.0.0", "0.1.2.3", "10.1.1.1", "100.64.1.1", "127.0.0.1", "169.254.169.254", "172.31.1.1", "192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.1.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:8.8.8.8", "64:ff9b::808:808", "100::1", "2001::1", "2001:db8::1", "2002::1", "3fff::1", "fc00::1", "fe80::1", "ff02::1", "4000::1"} {
		if safeRegistryAddress(netip.MustParseAddr(s)) {
			t.Errorf("unsafe address accepted %s", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !safeRegistryAddress(netip.MustParseAddr(s)) {
			t.Errorf("public address denied %s", s)
		}
	}
	for _, ips := range [][]netip.Addr{nil, {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, {netip.MustParseAddr("::ffff:8.8.8.8")}} {
		if _, err := resolveRegistryAddresses(context.Background(), restoreFixtureHost, func(context.Context, string) ([]netip.Addr, error) { return ips, nil }); err == nil {
			t.Fatal("unsafe DNS set accepted")
		}
	}
}
func TestRestoreAddressRebinding(t *testing.T) {
	f := newRestoreFixture(t, nil)
	calls := 0
	f.broker.resolve = func(context.Context, string) ([]netip.Addr, error) {
		calls++
		if calls == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	client := f.client()
	defer client.CloseIdleConnections()
	_, _ = fixtureRead(t, client, "GET", "https://registry.example/a", nil)
	r, err := client.Get("https://registry.example/b")
	if err == nil {
		_ = r.Body.Close()
		t.Fatal("rebound address accepted")
	}
	if f.dials.Load() != 1 || calls != 2 {
		t.Fatalf("dials=%d resolutions=%d", f.dials.Load(), calls)
	}
}
func TestRestoreConnectRejectedBeforeDial(t *testing.T) {
	requests := []string{
		"CONNECT registry.example:443 HTTP/1.1\r\nHost: cdn.example:443\r\n\r\n",
		"CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n",
		"GET http://registry.example/ HTTP/1.1\r\nHost: registry.example\r\n\r\n",
		"CONNECT registry.example:443 HTTP/1.1\r\nHost: registry.example:443\r\nProxy-Authorization: Basic SYNTHETIC\r\n\r\n",
		"\x16\x03\x03not CONNECT\r\n\r\n",
	}
	for _, host := range []string{"cdn.example", "cdn.example:443", "evil.example", "registry.example.evil", "evilregistry.example", "registry.example:80", "registry.example:444", "registry.example:0443", "registry.example:+443", "registry.example:", "registry.example::443", "registry.example,cdn.example", "", "user@registry.example", "registry%2eexample", "registry.example.", "Registry.example", "127.0.0.1", "[::1]", "registry. example", "registry.example\r\n folded"} {
		requests = append(requests, "CONNECT registry.example:443 HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	}
	for _, target := range []string{"registry.example", "registry.example:80", "registry.example:0443", "registry.example:+443", "user@registry.example:443", "cdn.example:443", "evil.example:443"} {
		requests = append(requests, "CONNECT "+target+" HTTP/1.1\r\nHost: registry.example\r\n\r\n")
	}
	for _, header := range []string{"Authorization: Basic SYNTHETIC", "Proxy-Authorization: Basic SYNTHETIC", "Cookie: SYNTHETIC", "User-Agent: one\r\nUser-Agent: two", "Accept: */*\r\nAccept: */*", "Connection: upgrade", "Upgrade: websocket", "Host: registry.example", "hOsT: cdn.example", "Host: registry.example:443", "Content-Length: 0", "Transfer-Encoding: chunked", "Bad Header: value"} {
		requests = append(requests, "CONNECT registry.example:443 HTTP/1.1\r\nHost: registry.example\r\n"+header+"\r\n\r\n")
	}
	requests = append(requests, "CONNECT registry.example:443 HTTP/1.1\r\n\r\n")
	for _, request := range requests {
		t.Run(request, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			c, err := f.listener.dial(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(c, request); err != nil {
				t.Fatal(err)
			}
			if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
				t.Fatal("invalid CONNECT accepted")
			}
			select {
			case <-f.broker.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("broker did not cancel")
			}
			f.mu.Lock()
			seen := len(f.seen)
			f.mu.Unlock()
			if seen != 0 {
				t.Fatal("rejected request reached upstream")
			}
			if f.dials.Load() != 0 {
				t.Fatal("CONNECT caused upstream dial")
			}
		})
	}
}
func TestRestoreCanaryTransport(t *testing.T) {
	f := newRestoreFixture(t, nil)
	f.broker.preflight.Store(true)
	client := f.client()
	defer client.CloseIdleConnections()
	_, data := fixtureRead(t, client, "GET", "https://"+restoreCanaryHost+"/canary", nil)
	if string(data) != "SWARM_RESTORE_OK" || f.dials.Load() != 0 {
		t.Fatal("preflight used outbound transport")
	}
	f.broker.preflight.Store(false)
	r, err := client.Get("https://" + restoreCanaryHost + "/canary")
	if err == nil {
		_ = r.Body.Close()
		t.Fatal("canary remained accessible after preflight")
	}
}
func TestRestoreCleanupAndFailClosed(t *testing.T) {
	for i := 0; i < 10; i++ {
		f := newRestoreFixture(t, nil)
		c, err := f.listener.dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(c, "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n")
		select {
		case <-f.broker.ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("policy failure did not cancel run")
		}
		if err = f.broker.close(); err == nil {
			t.Fatal("policy failure ignored")
		}
		if _, err = f.listener.dial(context.Background()); err == nil {
			t.Fatal("listener survived close")
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err = c.Read(make([]byte, 1)); err == nil {
			t.Fatal("active socket survived")
		}
		_ = c.Close()
	}
}
func TestRestoreCleanupPartialHandshake(t *testing.T) {
	f := newRestoreFixture(t, nil)
	c, err := f.listener.dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT registry.example:443 HTTP/1.1\r\nHost:")
	done := make(chan error, 1)
	go func() { done <- f.broker.close() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("partial handshake blocked teardown")
	}
}
func TestRestoreBoundedBody(t *testing.T) {
	for _, test := range []struct{ left, total int64 }{{2, 0}, {99, restoreAggregateLimit}} {
		b := &registryBroker{}
		b.bytes.Store(test.total)
		r := &restoreCountingBody{ReadCloser: io.NopCloser(strings.NewReader("artifact")), b: b, left: test.left}
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("body budget ignored")
		}
	}
}
func TestRestoreSourceSnapshot(t *testing.T) {
	root := t.TempDir()
	srcPath := filepath.Join(root, "input")
	dstPath := filepath.Join(root, "snapshot")
	if err := os.Mkdir(srcPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dstPath, 0700); err != nil {
		t.Fatal(err)
	}
	src, err := openRestoreDirectory(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := openRestoreDirectory(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = src.WriteFile("nested/build", []byte("approved"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyRestoreSource(src, dst, "", new(int64)); err != nil {
		t.Fatal(err)
	}
	if err = src.WriteFile("nested/build", []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := dst.ReadFile("nested/build", 64)
	if err != nil || string(data) != "approved" {
		t.Fatal("snapshot not detached", err)
	}
	for _, test := range []string{"symlink", "git", "hardlink"} {
		t.Run(test, func(t *testing.T) {
			path := t.TempDir()
			r, err := openRestoreDirectory(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			switch test {
			case "symlink":
				err = os.Symlink(srcPath, filepath.Join(path, "escape"))
			case "git":
				err = r.WriteFile("nested/.git/config", []byte("token"), 0600)
			case "hardlink":
				err = os.Link(filepath.Join(srcPath, "nested/build"), filepath.Join(path, "alias"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = copyRestoreSource(r, dst, "", new(int64)); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}
func TestRestoreInputSafety(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	source := filepath.Join(root, "source")
	cache := filepath.Join(repo, "cache")
	for _, p := range []string{repo, source, cache} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(root, "helper")
	syscall.ForkLock.RLock()
	err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 99\n"), 0700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	request := RestoreRequest{Source: source, RepositoryRoot: repo, CacheRoot: cache, Registries: []string{restoreFixtureHost}, Argv: []string{"/usr/bin/true"}}
	tests := []struct {
		name   string
		change func(*RestoreRequest, *RestoreBoundary)
	}{
		{"outside cache", func(r *RestoreRequest, b *RestoreBoundary) { r.CacheRoot = root }},
		{"overlap", func(r *RestoreRequest, b *RestoreBoundary) { r.Source = repo }},
		{"relative", func(r *RestoreRequest, b *RestoreBoundary) { r.Source = "source" }},
		{"traversal", func(r *RestoreRequest, b *RestoreBoundary) { r.Source = source + "/../source" }},
		{"helper missing", func(r *RestoreRequest, b *RestoreBoundary) { b.Helper = filepath.Join(root, "absent") }},
		{"argv", func(r *RestoreRequest, b *RestoreBoundary) { r.Argv = []string{"true"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := request
			b := RestoreBoundary{Helper: helper}
			test.change(&r, &b)
			if _, err := b.Run(context.Background(), r); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if r, err := openRestoreDirectory(link); err == nil {
		r.Close()
		t.Fatal("symlink directory accepted")
	}
	if f, err := openRestoreRegular(filepath.Join(link, "file")); err == nil {
		f.Close()
		t.Fatal("symlink ancestor accepted")
	}
}
func TestRestoreRedirectPolicy(t *testing.T) {
	b := &registryBroker{policy: registryPolicy{restoreFixtureHost: true, restoreFixtureCDN: true}}
	base, _ := url.Parse("https://registry.example/a/b")
	for _, s := range []string{"/c", "../c", "https://cdn.example/c", "https://registry.example:443/c"} {
		if err := b.validateRedirect(base, s); err != nil {
			t.Fatalf("allowed %q: %v", s, err)
		}
	}
	for _, s := range []string{"", "http://registry.example/c", "https://evil.example/c", "https://registry.example./c", "https://registry.example:0443/c", "https://registry.example/c?token=x", "https://registry.example/c#x", "https://user:pass@registry.example/c", "https://registry.example/c%3fsecret=x"} {
		if err := b.validateRedirect(base, s); err == nil {
			t.Fatalf("unsafe redirect %q", s)
		}
	}
}
func TestRestoreProtocolMalformedChunkedAndTruncation(t *testing.T) {
	for _, raw := range []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\nSet-Cookie: forbidden\r\n\r\n", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nZZ\r\nx\r\n0\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort"} {
		t.Run(raw, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			f.broker.dial = func(context.Context, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				go func() {
					defer a.Close()
					cert := f.upstream.TLS
					tc := tls.Server(a, cert)
					reader := bufio.NewReader(tc)
					if _, err := http.ReadRequest(reader); err != nil {
						return
					}
					_, _ = io.WriteString(tc, raw)
				}()
				return b, nil
			}
			c := f.client()
			defer c.CloseIdleConnections()
			r, err := c.Get("https://registry.example/a")
			if err == nil {
				_, err = io.ReadAll(r.Body)
				_ = r.Body.Close()
			}
			if err == nil {
				t.Fatal("malformed upstream framing accepted")
			}
		})
	}
}

func TestRestoreHelperBridgeProtocol(t *testing.T) {
	f := newRestoreFixture(t, nil)
	brokerListener := f.listener
	helperListener := newRestoreTestListener()
	bridge := startRestoreBridge(t.Context(), helperListener, brokerListener.dial)
	t.Cleanup(func() { _ = bridge.close() })
	f.listener = helperListener
	client := f.client()
	defer client.CloseIdleConnections()
	_, data := fixtureRead(t, client, "GET", "https://registry.example/artifact", nil)
	if string(data) != "artifact" || f.dials.Load() != 1 {
		t.Fatal("helper failed native proxy transport")
	}
	bridge.close()
	if c, err := helperListener.dial(context.Background()); err == nil {
		c.Close()
		t.Fatal("helper listener survived cleanup")
	}
}
func TestRestoreBrokerConnectionBounds(t *testing.T) {
	f := newRestoreFixture(t, nil)
	var clients []net.Conn
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < 17; i++ {
		c, err := f.listener.dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	select {
	case <-f.broker.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("connection flood not cancelled")
	}
	if err := f.broker.close(); err == nil {
		t.Fatal("flood reported success")
	}
}
func TestRestoreBrokerSessionBounds(t *testing.T) {
	f := newRestoreFixture(t, nil)
	var clients []*tls.Conn
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < 9; i++ {
		c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, nil)
		if err := c.Handshake(); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	select {
	case <-f.broker.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("session flood not cancelled")
	}
}
func TestRestoreClientHelloLimit(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &restoreHandshakeConn{Conn: left, left: 4}
	done := make(chan struct{})
	go func() { _, _ = right.Write([]byte("oversize")); close(done) }()
	data := make([]byte, 16)
	n, err := conn.Read(data)
	if n != 4 || err != nil {
		t.Fatal(n, err)
	}
	if _, err = conn.Read(data); err == nil {
		t.Fatal("handshake budget bypass")
	}
	_ = left.Close()
	<-done
}
func TestRestoreCapabilityReasons(t *testing.T) {
	for status, want := range map[string]string{"NETWORK\n": "direct-network canary", "FILESYSTEM\n": "filesystem canary", "BROKER\n": "CONNECT/TLS broker canary", "LISTENER\n": "loopback listener", "TRUST\n": "public trust artifacts", "SYNTHETIC_SECRET\n": "helper startup/control framing"} {
		if got := safeRestoreStatus(status); got != want {
			t.Fatalf("%q => %q", status, got)
		}
	}
	for diagnostic, want := range map[string]string{"No permissions to create a new namespace": "namespaces", "Can't bind mount SYNTHETIC_SECRET": "mounts", "unknown SYNTHETIC_SECRET": "helper startup/control framing"} {
		got := classifyRestoreLauncher(diagnostic)
		if !strings.HasSuffix(got, want) || strings.Contains(got, "SYNTHETIC_SECRET") {
			t.Fatalf("unsafe/unhelpful diagnostic %q", got)
		}
	}
}
func TestRestoreOutputRootPinned(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "cache")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	pinned, err := openRestoreDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	root, name, err := createRestoreOutput(pinned, path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := filepath.Join(parent, "moved")
	if err = os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err = createRestoreOutput(pinned, path); err == nil {
		t.Fatal("replaced cache accepted")
	}
	if err = root.RemoveAll(name); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(moved, name)); !os.IsNotExist(err) {
		t.Fatal("cleanup did not use pinned output root", err)
	}
}

func TestRestoreBrokerStallCancellation(t *testing.T) {
	f := newRestoreFixture(t, nil)
	started := make(chan struct{})
	f.broker.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	client := f.client()
	defer client.CloseIdleConnections()
	done := make(chan error, 1)
	go func() {
		r, err := client.Get("https://registry.example/a")
		if err == nil {
			r.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not start")
	}
	_ = f.broker.close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled upstream succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt stalled upstream")
	}
}

type restoreCloseFailure struct{ net.Listener }

func (l restoreCloseFailure) Close() error { _ = l.Listener.Close(); return io.ErrUnexpectedEOF }
func TestRestoreCleanupErrors(t *testing.T) {
	l := newRestoreTestListener()
	trust, err := newRestoreTrust([]string{restoreFixtureHost})
	if err != nil {
		t.Fatal(err)
	}
	b := newRegistryBroker(t.Context(), restoreCloseFailure{l}, registryPolicy{restoreFixtureHost: true}, trust)
	b.serve()
	if err = b.close(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("broker cleanup failure suppressed", err)
	}
	bridge := startRestoreBridge(t.Context(), restoreCloseFailure{newRestoreTestListener()}, nil)
	if err = bridge.close(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("helper cleanup failure suppressed", err)
	}
}

func TestRestoreReadyFrame(t *testing.T) {
	for _, tc := range []struct {
		name, frame, want string
	}{
		{"ready", "READY\n", "READY\n"},
		{"failure", "NETWORK\n", "NETWORK\n"},
		{"crlf", "READY\r\n", "READY\r\n"},
		{"prefix", "xREADY\n", "xREADY\n"},
		{"partial", "READY", ""},
		{"empty", "", ""},
		{"oversized", strings.Repeat("x", 256) + "\n", ""},
		{"unterminated", strings.Repeat("x", 512), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readRestoreReadyFrame(strings.NewReader(tc.frame)); got != tc.want {
				t.Fatalf("frame = %q, want %q", got, tc.want)
			}
			if tc.name != "ready" && readRestoreReadyFrame(strings.NewReader(tc.frame)) == "READY\n" {
				t.Fatal("malformed frame accepted as ready")
			}
		})
	}
}

func TestRestoreReadyFrameCancellation(t *testing.T) {
	for _, prefix := range []string{"", "REA"} {
		t.Run(prefix, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			if _, err := io.WriteString(w, prefix); err != nil {
				t.Fatal(err)
			}
			result := make(chan string, 1)
			go func() { result <- readRestoreReadyFrame(r) }()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			<-ctx.Done()
			// The lifecycle owner closes the reader on cancellation or deadline.
			_ = r.Close()
			select {
			case got := <-result:
				if got != "" {
					t.Fatalf("cancelled frame = %q", got)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled reader did not stop")
			}
		})
	}
}

func TestRestoreHomeContainedRepository(t *testing.T) {
	// Model a normal home checkout without requiring writes to the host's home.
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "src", "SwarmOS")
	cache := filepath.Join(repo, ".swarm", "private-cache")
	for _, path := range []string{repo, cache} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := validateRestoreRoot(path, home); err != nil {
			t.Fatalf("home-contained repository rejected: %v", err)
		}
		pinned, err := openRestoreDirectory(path)
		if err != nil {
			t.Fatal(err)
		}
		pinned.Close()
	}
	source := filepath.Join(home, "approved-source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := (RestoreBoundary{Helper: filepath.Join(home, "missing-helper")}).Run(t.Context(), RestoreRequest{
		Source: source, RepositoryRoot: repo, CacheRoot: cache,
		Registries: []string{restoreFixtureHost}, Argv: []string{"/bin/true"},
	})
	if err == nil || !strings.Contains(err.Error(), "restore helper unavailable") {
		t.Fatalf("home-contained paths did not reach helper validation: %v", err)
	}
	for _, path := range []string{"/home/byron/src/SwarmOS", "/home/byron/src/SwarmOS/.cache", "/root/src/repo"} {
		if err := validateRestoreRoot(path, "/home/byron"); err != nil {
			t.Fatalf("private child %q rejected: %v", path, err)
		}
	}
	for _, path := range []string{home, "/", "/home", "/root", "/tmp", "/var", "/usr", "/usr/local", "/etc/private", "/proc/self"} {
		if err := validateRestoreRoot(path, home); err == nil {
			t.Fatalf("unsafe root %q accepted", path)
		}
	}
	// A repository-owned spelling cannot hide a symlink to another home path.
	link := filepath.Join(repo, "linked-cache")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	if pinned, err := openRestoreDirectory(link); err == nil {
		pinned.Close()
		t.Fatal("linked cache accepted")
	}
	for _, source := range []string{repo, cache, filepath.Join(cache, "source")} {
		if !restoreOverlap(source, cache) {
			t.Fatalf("overlap missed: %q", source)
		}
	}
}

// restoreAdmissionListener deliberately returns one successful Accept after Close.
// Its release is owned by the test, never by cancellation or a timer.
type restoreAdmissionListener struct {
	net.Listener
	conn                     net.Conn
	entered, release, closed chan struct{}
	once                     sync.Once
	accepts                  atomic.Int64
}

func (l *restoreAdmissionListener) Accept() (net.Conn, error) {
	if l.accepts.Add(1) == 1 {
		close(l.entered)
		<-l.release
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *restoreAdmissionListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

type restoreObservedConn struct {
	net.Conn
	once             sync.Once
	closed, reading  chan struct{}
	readOnce         sync.Once
	deadlines, reads atomic.Int64
	closeErr         error
}

func (c *restoreObservedConn) Close() error {
	c.once.Do(func() { _ = c.Conn.Close(); close(c.closed) })
	return c.closeErr
}
func (c *restoreObservedConn) Read(p []byte) (int, error) {
	c.reads.Add(1)
	c.readOnce.Do(func() { close(c.reading) })
	return c.Conn.Read(p)
}
func (c *restoreObservedConn) SetDeadline(d time.Time) error {
	c.deadlines.Add(1)
	return c.Conn.SetDeadline(d)
}
func restoreObservedPipe(t *testing.T) (*restoreObservedConn, net.Conn) {
	t.Helper()
	a, b, err := newRestoreTestPipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &restoreObservedConn{Conn: a, closed: make(chan struct{}), reading: make(chan struct{})}
	t.Cleanup(func() { _ = c.Close(); _ = b.Close() })
	return c, b
}
func restoreAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(what)
	}
}

type restoreLifecycleTest struct {
	mu       *sync.Mutex
	active   map[net.Conn]bool
	accepted *atomic.Int64
	cancel   context.CancelFunc
	close    func() error
	wait     func()
	fail     func()
}

func restoreLifecycle(t *testing.T, kind string, l net.Listener, dial func(context.Context) (net.Conn, error)) restoreLifecycleTest {
	t.Helper()
	if kind == "broker" {
		b := newRegistryBroker(t.Context(), l, nil, nil)
		b.serve()
		return restoreLifecycleTest{&b.mu, b.active, &b.accepted, b.cancel, b.close, b.wg.Wait, b.fail}
	}
	b := startRestoreBridge(t.Context(), l, dial)
	return restoreLifecycleTest{&b.mu, b.active, &b.accepted, b.cancel, b.close, b.wg.Wait, nil}
}
func (h restoreLifecycleTest) assertEmpty(t *testing.T, sentinel net.Conn) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.active, sentinel)
	if len(h.active) != 0 || h.accepted.Load() != 0 {
		t.Errorf("remaining connections=%d accepted=%d", len(h.active), h.accepted.Load())
	}
}

// The sentinel signal followed by this mutex acquisition proves the old
// locked drain has finished. The sentinel has no worker that could remove it.
func (h restoreLifecycleTest) drainBarrier(t *testing.T, sentinel net.Conn) {
	t.Helper()
	h.mu.Lock()
	present := h.active[sentinel]
	h.mu.Unlock()
	if !present {
		t.Fatal("drain sentinel unexpectedly removed")
	}
}

func TestRestoreShutdownAdmission(t *testing.T) {
	for _, kind := range []string{"broker", "bridge"} {
		t.Run(kind, func(t *testing.T) {
			client, peer := restoreObservedPipe(t)
			if kind == "broker" {
				if _, err := io.WriteString(peer, "CONNECT registry.example:443 HTTP/1.1\r\nHost:"); err != nil {
					t.Fatal(err)
				}
			}
			upstream, _ := restoreObservedPipe(t)
			sentinel, _ := restoreObservedPipe(t)
			l := &restoreAdmissionListener{conn: client, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(l.release) }) }
			var dials atomic.Int64
			h := restoreLifecycle(t, kind, l, func(context.Context) (net.Conn, error) { dials.Add(1); return upstream, nil })
			done := make(chan struct{})
			started := false
			// Cleanup rescues the old blocked header without using peer closure for success.
			cleanup := func() {
				unblock()
				h.cancel()
				_ = l.Close()
				_ = client.Close()
				_ = upstream.Close()
				_ = sentinel.Close()
				_ = h.close()
				h.wait()
				if started {
					restoreAwait(t, done, "close worker survived cleanup")
				}
				h.assertEmpty(t, sentinel)
			}
			t.Cleanup(cleanup)
			restoreAwait(t, l.entered, "Accept not entered")
			h.mu.Lock()
			h.active[sentinel] = true
			h.mu.Unlock()
			started = true
			go func() { _ = h.close(); close(done) }()
			restoreAwait(t, sentinel.closed, "sentinel not closed")
			h.drainBarrier(t, sentinel)
			unblock()
			completed := false
			select {
			case <-done:
				completed = true
			case <-time.After(time.Second):
				t.Error("partial handshake blocked teardown")
			}
			if !completed {
				cleanup()
			}
			if dials.Load() != 0 {
				t.Errorf("late client invoked dial: %d", dials.Load())
			}
			restoreAwait(t, client.closed, "late client not closed")
			h.assertEmpty(t, sentinel)
		})
	}
}
func TestRestoreShutdownLateUpstream(t *testing.T) {
	client, _ := restoreObservedPipe(t)
	upstream, _ := restoreObservedPipe(t)
	sentinel, _ := restoreObservedPipe(t)
	l := newRestoreTestListener()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	h := restoreLifecycle(t, "bridge", l, func(context.Context) (net.Conn, error) { close(entered); <-release; return upstream, nil })
	done := make(chan struct{})
	started := false
	t.Cleanup(func() {
		unblock()
		h.cancel()
		_ = l.Close()
		_ = client.Close()
		_ = upstream.Close()
		_ = sentinel.Close()
		_ = h.close()
		h.wait()
		if started {
			restoreAwait(t, done, "close worker survived cleanup")
		}
		h.assertEmpty(t, sentinel)
	})
	select {
	case l.pending <- client:
	case <-time.After(time.Second):
		t.Fatal("client not accepted")
	}
	restoreAwait(t, entered, "dial not entered")
	h.mu.Lock()
	h.active[sentinel] = true
	h.mu.Unlock()
	started = true
	go func() { _ = h.close(); close(done) }()
	restoreAwait(t, sentinel.closed, "sentinel not closed")
	h.drainBarrier(t, sentinel)
	unblock()
	restoreAwait(t, done, "late upstream blocked teardown")
	restoreAwait(t, upstream.closed, "late upstream not closed")
	if upstream.deadlines.Load() != 0 || upstream.reads.Load() != 0 {
		t.Errorf("late upstream forwarding setup=%d reads=%d", upstream.deadlines.Load(), upstream.reads.Load())
	}
	h.assertEmpty(t, sentinel)
}

func TestRestoreShutdownConcurrentClose(t *testing.T) {
	for _, kind := range []string{"broker", "bridge"} {
		t.Run(kind, func(t *testing.T) {
			// This socket has no worker, so its cleanup error must come from the snapshot.
			sentinel, _ := restoreObservedPipe(t)
			sentinel.closeErr = io.ErrShortWrite
			l := restoreCloseFailure{newRestoreTestListener()}
			h := restoreLifecycle(t, kind, l, nil)
			t.Cleanup(func() { _ = h.close() })
			h.mu.Lock()
			h.active[sentinel] = true
			h.mu.Unlock()
			const callers = 8
			results := make(chan error, callers)
			for i := 0; i < callers; i++ {
				go func() { results <- h.close() }()
			}
			for i := 0; i < callers; i++ {
				select {
				case err := <-results:
					if !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Errorf("lost cleanup error: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("concurrent close blocked")
				}
			}
			if err := h.close(); !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Error("repeated close lost error", err)
			}
			restoreAwait(t, sentinel.closed, "socket not closed")
			h.assertEmpty(t, sentinel)
		})
	}
}

func TestRestoreShutdownCancellation(t *testing.T) {
	for _, kind := range []string{"broker", "bridge"} {
		t.Run(kind, func(t *testing.T) {
			client, _ := restoreObservedPipe(t)
			sentinel, _ := restoreObservedPipe(t)
			l := &restoreAdmissionListener{conn: client, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
			var once sync.Once
			unblock := func() { once.Do(func() { close(l.release) }) }
			var dials atomic.Int64
			h := restoreLifecycle(t, kind, l, func(context.Context) (net.Conn, error) { dials.Add(1); return nil, io.EOF })
			t.Cleanup(func() { unblock(); h.cancel(); _ = client.Close(); _ = h.close() })
			restoreAwait(t, l.entered, "Accept not entered")
			h.mu.Lock()
			h.active[sentinel] = true
			h.mu.Unlock()
			h.cancel()
			restoreAwait(t, sentinel.closed, "cancellation did not drain sockets")
			h.drainBarrier(t, sentinel)
			unblock()
			done := make(chan struct{})
			go func() { _ = h.close(); close(done) }()
			restoreAwait(t, done, "canceled admission blocked close")
			restoreAwait(t, client.closed, "canceled client not closed")
			if dials.Load() != 0 {
				t.Error("canceled client reached dial")
			}
			h.assertEmpty(t, sentinel)
		})
	}
	t.Run("dial", func(t *testing.T) {
		client, _ := restoreObservedPipe(t)
		upstream, _ := restoreObservedPipe(t)
		sentinel, _ := restoreObservedPipe(t)
		l := newRestoreTestListener()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		h := restoreLifecycle(t, "bridge", l, func(context.Context) (net.Conn, error) { close(entered); <-release; return upstream, nil })
		t.Cleanup(func() { unblock(); h.cancel(); _ = client.Close(); _ = upstream.Close(); _ = h.close() })
		select {
		case l.pending <- client:
		case <-time.After(time.Second):
			t.Fatal("Accept blocked")
		}
		restoreAwait(t, entered, "dial not entered")
		h.mu.Lock()
		h.active[sentinel] = true
		h.mu.Unlock()
		h.cancel()
		restoreAwait(t, sentinel.closed, "cancellation did not drain")
		h.drainBarrier(t, sentinel)
		unblock()
		done := make(chan struct{})
		go func() { _ = h.close(); close(done) }()
		restoreAwait(t, done, "canceled dial blocked close")
		restoreAwait(t, upstream.closed, "canceled dial socket not closed")
		if upstream.deadlines.Load() != 0 || upstream.reads.Load() != 0 {
			t.Error("canceled dial started forwarding")
		}
		h.assertEmpty(t, sentinel)
	})
}

func TestRestoreShutdownPolicyFailure(t *testing.T) {
	client, _ := restoreObservedPipe(t)
	sentinel, _ := restoreObservedPipe(t)
	l := &restoreAdmissionListener{conn: client, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(l.release) }) }
	h := restoreLifecycle(t, "broker", l, nil)
	t.Cleanup(func() { unblock(); h.cancel(); _ = client.Close(); _ = h.close() })
	restoreAwait(t, l.entered, "Accept not entered")
	h.mu.Lock()
	h.active[sentinel] = true
	h.mu.Unlock()
	failed := make(chan struct{})
	go func() { h.fail(); close(failed) }()
	restoreAwait(t, sentinel.closed, "policy failure did not drain")
	h.drainBarrier(t, sentinel)
	unblock()
	restoreAwait(t, failed, "failure waited on worker")
	done := make(chan struct{})
	var result error
	go func() { result = h.close(); close(done) }()
	restoreAwait(t, done, "policy failure blocked close")
	if result == nil || !strings.Contains(result.Error(), "rejected transport") {
		t.Error("lost policy error", result)
	}
	restoreAwait(t, client.closed, "late client survived policy failure")
	h.assertEmpty(t, sentinel)
}

func TestRestoreShutdownIdleCopies(t *testing.T) {
	client, _ := restoreObservedPipe(t)
	upstream, _ := restoreObservedPipe(t)
	l := newRestoreTestListener()
	h := restoreLifecycle(t, "bridge", l, func(context.Context) (net.Conn, error) { return upstream, nil })
	t.Cleanup(func() { _ = client.Close(); _ = upstream.Close(); _ = h.close() })
	select {
	case l.pending <- client:
	case <-time.After(time.Second):
		t.Fatal("Accept blocked")
	}
	restoreAwait(t, client.reading, "client copy not reading")
	restoreAwait(t, upstream.reading, "upstream copy not reading")
	done := make(chan struct{})
	go func() { _ = h.close(); close(done) }()
	restoreAwait(t, done, "idle copies survived close")
	restoreAwait(t, client.closed, "client not closed")
	restoreAwait(t, upstream.closed, "upstream not closed")
	h.assertEmpty(t, nil)
}

func TestRestoreShutdownDialFailure(t *testing.T) {
	client, _ := restoreObservedPipe(t)
	l := newRestoreTestListener()
	h := restoreLifecycle(t, "bridge", l, func(context.Context) (net.Conn, error) { return nil, io.ErrUnexpectedEOF })
	t.Cleanup(func() { _ = client.Close(); _ = h.close() })
	select {
	case l.pending <- client:
	case <-time.After(time.Second):
		t.Fatal("Accept blocked")
	}
	restoreAwait(t, l.closed, "dial failure did not stop listener")
	done := make(chan struct{})
	go func() { _ = h.close(); close(done) }()
	restoreAwait(t, done, "dial failure blocked close")
	restoreAwait(t, client.closed, "dial failure leaked client")
	h.assertEmpty(t, nil)
}

func TestRestoreShutdownBridgeConnectionBounds(t *testing.T) {
	l := newRestoreTestListener()
	entered := make(chan struct{}, 16)
	h := restoreLifecycle(t, "bridge", l, func(ctx context.Context) (net.Conn, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var clients []*restoreObservedConn
	t.Cleanup(func() {
		for _, c := range clients {
			_ = c.Close()
		}
		_ = h.close()
	})
	for i := 0; i < 17; i++ {
		c, _ := restoreObservedPipe(t)
		clients = append(clients, c)
		select {
		case l.pending <- c:
		case <-time.After(time.Second):
			t.Fatal("Accept blocked")
		}
		if i < 16 {
			restoreAwait(t, entered, "admitted client did not dial")
		}
	}
	restoreAwait(t, l.closed, "connection limit did not stop listener")
	done := make(chan struct{})
	go func() { _ = h.close(); close(done) }()
	restoreAwait(t, done, "limit cleanup blocked")
	for _, c := range clients {
		restoreAwait(t, c.closed, "client survived limit cleanup")
	}
	h.assertEmpty(t, nil)
}
