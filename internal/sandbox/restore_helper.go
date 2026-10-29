package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// RunRestoreHelper is only called by the internal helper binary. The broker
// destination and trust paths are fixed; argv never controls bridge routing.
func RunRestoreHelper(args []string) (retErr error) {
	if len(args) < 2 || args[1] != "--" {
		// A fixed frame lets startup tests distinguish argument validation from
		// runtime failure without exposing arguments or runtime diagnostics.
		_, _ = io.WriteString(os.Stdout, "ARGUMENTS\n")
		return errors.New("restore helper arguments")
	}
	control := os.Stdout
	ack := os.Stdin
	if err := restoreCloseDescriptors(); err != nil {
		return err
	}
	if control == nil || ack == nil {
		return errors.New("restore helper control unavailable")
	}
	defer control.Close()
	defer ack.Close()
	status := func(s string) error {
		_, _ = io.WriteString(control, s+"\n")
		_ = control.Close()
		return errors.New("restore helper preflight failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bridge, err := newRestoreBridge(ctx)
	if err != nil {
		return status("LISTENER")
	}
	defer func() { retErr = errors.Join(retErr, bridge.close()) }()
	ca, err := os.ReadFile("/restore-trust/ca.pem")
	if err != nil {
		return status("TRUST")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return status("TRUST")
	}
	if err = restoreHelperFilesystemCanary(); err != nil {
		return status("FILESYSTEM")
	}
	// The host checked this exact address before launch. No route to host loopback
	// or public addresses is ever installed in this network namespace.
	interfaces, e := net.Interfaces()
	if e != nil {
		return status("NETWORK")
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			return status("NETWORK")
		}
	}
	for _, target := range []string{args[0], "1.1.1.1:443", "169.254.169.254:80", "[2606:4700:4700::1111]:443", "[::1]:9"} {
		c, e := net.DialTimeout("tcp", target, 200*time.Millisecond)
		if e == nil {
			_ = c.Close()
			return status("NETWORK")
		}
	}
	proxyURL := &url.URL{Scheme: "http", Host: bridge.listener.Addr().String()}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, DisableKeepAlives: true}
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://"+restoreCanaryHost+"/canary", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		transport.CloseIdleConnections()
		return status("BROKER")
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 32))
	_ = response.Body.Close()
	transport.CloseIdleConnections()
	if readErr != nil || response.StatusCode != 200 || string(data) != "SWARM_RESTORE_OK" {
		return status("BROKER")
	}
	if _, err = io.WriteString(control, "READY\n"); err != nil {
		return err
	}
	_ = control.Close()
	ackData, err := io.ReadAll(io.LimitReader(ack, 4))
	_ = ack.Close()
	if err != nil || string(ackData) != "GO\n" {
		return errors.New("restore helper acknowledgement")
	}
	if len(args) == 2 {
		return nil
	}
	port := strconv.Itoa(bridge.listener.Addr().(*net.TCPAddr).Port)
	command := exec.CommandContext(ctx, args[2], args[3:]...)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/cache/home", "TMPDIR=/cache/tmp", "XDG_CACHE_HOME=/cache/home/.cache", "XDG_CONFIG_HOME=/cache/home/.config", "XDG_DATA_HOME=/cache/home/.local/share",
		"HTTPS_PROXY=" + proxyURL.String(), "https_proxy=" + proxyURL.String(), "NO_PROXY=", "no_proxy=", "SSL_CERT_FILE=/restore-trust/ca.pem", "SSL_CERT_DIR=/restore-trust/empty",
		// Native JVM settings are part of the transport contract, not host options.
		"JAVA_TOOL_OPTIONS=-Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=" + port + " -Dhttp.nonProxyHosts= -Djavax.net.ssl.trustStore=/restore-trust/ca.jks -Djavax.net.ssl.trustStoreType=JKS -Djavax.net.ssl.trustStorePassword=" + restoreTrustPassword}
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 5 * time.Second
	done := make(chan error, 1)
	if err = command.Start(); err != nil {
		return errors.New("restore payload start failed")
	}
	go func() { done <- command.Wait() }()
	select {
	case err = <-done:
	case <-bridge.ctx.Done():
		_ = command.Process.Kill()
		<-done
		err = errors.New("restore bridge failed")
	}
	if err != nil {
		return errors.New("restore payload failed")
	}
	return nil
}
func restoreHelperFilesystemCanary() error {
	f, err := os.OpenFile("/cache/tmp/.canary", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_ = f.Close()
	if err = os.Remove("/cache/tmp/.canary"); err != nil {
		return err
	}
	for _, path := range []string{"/source/.restore-write-canary", "/.restore-write-canary", "/dev/.restore-write-canary", "/restore-broker/.restore-write-canary"} {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = f.Close()
			_ = os.Remove(path)
			return errors.New("restore write boundary failed")
		}
	}
	for _, path := range []string{"/home", "/root", "/proc/1/root/root", "/proc/1/root/home"} {
		if _, err = os.Stat(path); err == nil {
			return errors.New("restore host path visible")
		}
	}
	return nil
}

type restoreBridge struct {
	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	mu       sync.Mutex
	active   map[net.Conn]bool
	wg       sync.WaitGroup
	accepted atomic.Int64
	dial     func(context.Context) (net.Conn, error)
}

func newRestoreBridge(ctx context.Context) (*restoreBridge, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return startRestoreBridge(ctx, l, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", "/restore-broker/broker.sock")
	}), nil
}

func startRestoreBridge(ctx context.Context, l net.Listener, dial func(context.Context) (net.Conn, error)) *restoreBridge {
	ctx, cancel := context.WithCancel(ctx)
	b := &restoreBridge{ctx: ctx, cancel: cancel, listener: l, active: map[net.Conn]bool{}, dial: dial}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if b.accepted.Add(1) > 16 {
				b.accepted.Add(-1)
				_ = c.Close()
				b.cancel()
				return
			}
			b.mu.Lock()
			b.active[c] = true
			b.mu.Unlock()
			b.wg.Add(1)
			go b.forward(c)
		}
	}()
	return b
}
func (b *restoreBridge) forward(client net.Conn) {
	defer b.wg.Done()
	defer b.accepted.Add(-1)
	defer client.Close()
	defer func() { b.mu.Lock(); delete(b.active, client); b.mu.Unlock() }()
	upstream, err := b.dial(b.ctx)
	if err != nil {
		b.cancel()
		return
	}
	defer upstream.Close()
	b.mu.Lock()
	b.active[upstream] = true
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.active, upstream); b.mu.Unlock() }()
	_ = client.SetDeadline(time.Now().Add(5 * time.Minute))
	_ = upstream.SetDeadline(time.Now().Add(5 * time.Minute))
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
func (b *restoreBridge) close() error {
	b.cancel()
	err := restoreCloseError(b.listener.Close())
	b.mu.Lock()
	for c := range b.active {
		err = errors.Join(err, restoreCloseError(c.Close()))
	}
	b.mu.Unlock()
	b.wg.Wait()
	return err
}
