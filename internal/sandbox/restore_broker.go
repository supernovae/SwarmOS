package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	restoreHeaderLimit    = 16 << 10
	restoreURLLimit       = 8 << 10
	restoreArtifactLimit  = 256 << 20
	restoreAggregateLimit = 1 << 30
	restoreRequestLimit   = 1024
)

type registryPolicy map[string]bool

func newRegistryPolicy(hosts []string) (registryPolicy, error) {
	if len(hosts) == 0 || len(hosts) > 64 {
		return nil, errors.New("restore requires 1 to 64 registry hosts")
	}
	p := registryPolicy{}
	for _, h := range hosts {
		if !validRegistryHost(h) || h == restoreCanaryHost {
			return nil, errors.New("invalid restore registry policy")
		}
		p[h] = true
	}
	return p, nil
}
func validRegistryHost(h string) bool {
	if len(h) == 0 || len(h) > 253 || h != strings.ToLower(h) || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	// Numeric top-level labels cannot represent canonical public DNS names.
	last := h[strings.LastIndexByte(h, '.')+1:]
	for _, c := range last {
		if c >= 'a' && c <= 'z' {
			return true
		}
	}
	return false
}
func parseRegistryTarget(authority string, p registryPolicy) (string, error) {
	h, port, err := net.SplitHostPort(authority)
	if err != nil || port != "443" || authority != h+":443" || !validRegistryHost(h) || !p[h] {
		return "", errors.New("restore registry authority denied")
	}
	return h, nil
}

var restoreDeniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func safeRegistryAddress(a netip.Addr) bool {
	if !a.IsValid() || a.Is4In6() || a.Zone() != "" || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	// Public IPv6 currently comes from 2000::/3; reject unallocated space.
	if a.Is6() && !netip.MustParsePrefix("2000::/3").Contains(a) {
		return false
	}
	for _, p := range restoreDeniedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
func resolveRegistryAddresses(ctx context.Context, host string, resolve func(context.Context, string) ([]netip.Addr, error)) ([]netip.Addr, error) {
	ips, err := resolve(ctx, host)
	if err != nil || len(ips) == 0 || len(ips) > 32 {
		return nil, errors.New("restore registry resolution failed")
	}
	for _, ip := range ips {
		if !safeRegistryAddress(ip) {
			return nil, errors.New("restore registry address denied")
		}
	}
	return ips, nil
}

type registryBroker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	policy    registryPolicy
	trust     *restoreTrust
	resolve   func(context.Context, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	roots     *x509.CertPool // nil is system roots; tests supply independent fixture roots.
	mu        sync.Mutex
	active    map[net.Conn]bool
	err       error
	wg        sync.WaitGroup
	requests  atomic.Int64
	bytes     atomic.Int64
	accepted  atomic.Int64
	sessions  chan struct{}
	preflight atomic.Bool
}

func newRegistryBroker(ctx context.Context, l net.Listener, p registryPolicy, t *restoreTrust) *registryBroker {
	ctx, cancel := context.WithCancel(ctx)
	b := &registryBroker{ctx: ctx, cancel: cancel, listener: l, policy: p, trust: t, active: map[net.Conn]bool{}, sessions: make(chan struct{}, 8)}
	b.resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
	}
	b.dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	return b
}
func (b *registryBroker) fail() {
	b.mu.Lock()
	if b.err == nil {
		b.err = errors.New("restore broker rejected transport")
	}
	b.mu.Unlock()
	b.cancel()
	_ = b.listener.Close()
	b.mu.Lock()
	for c := range b.active {
		_ = c.Close()
	}
	b.mu.Unlock()
}
func (b *registryBroker) serve() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := b.listener.Accept()
			if err != nil {
				if b.ctx.Err() == nil {
					b.fail()
				}
				return
			}
			if b.accepted.Add(1) > 16 {
				b.accepted.Add(-1)
				_ = c.Close()
				b.fail()
				return
			}
			b.mu.Lock()
			b.active[c] = true
			b.mu.Unlock()
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				defer func() { b.accepted.Add(-1); _ = c.Close(); b.mu.Lock(); delete(b.active, c); b.mu.Unlock() }()
				if err := b.serveRestoreConnect(c); err != nil && b.ctx.Err() == nil {
					b.fail()
				}
			}()
		}
	}()
}
func (b *registryBroker) close() error {
	b.cancel()
	closeErr := restoreCloseError(b.listener.Close())
	b.mu.Lock()
	for c := range b.active {
		closeErr = errors.Join(closeErr, restoreCloseError(c.Close()))
	}
	b.mu.Unlock()
	b.wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	return errors.Join(b.err, closeErr)
}

// Read one header without reading past its boundary (TLS bytes may follow CONNECT).
func readRestoreHeader(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for len(out) < restoreHeaderLimit {
		c, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		if len(out) >= 4 && string(out[len(out)-4:]) == "\r\n\r\n" {
			return out, nil
		}
	}
	return nil, errors.New("restore header limit")
}
func parseRestoreRequest(r *bufio.Reader) (*http.Request, error) {
	raw, err := readRestoreHeader(r)
	if err != nil {
		return nil, err
	}
	// Reject obsolete folding, bare newlines and ambiguous whitespace before net/http normalizes it.
	lines := strings.Split(string(raw), "\r\n")
	hostCount := 0
	rawHost := ""
	lengths := 0
	for i, line := range lines {
		if strings.ContainsAny(line, "\r\n\x00") {
			return nil, errors.New("restore malformed framing")
		}
		if i > 0 && line != "" {
			if line[0] == ' ' || line[0] == '\t' {
				return nil, errors.New("restore folded header")
			}
			k, _, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(k) != k {
				return nil, errors.New("restore malformed header")
			}
			if strings.EqualFold(k, "Content-Length") {
				lengths++
				if lengths > 1 {
					return nil, errors.New("restore duplicate length")
				}
			}
			if strings.EqualFold(k, "Host") {
				hostCount++
				_, value, _ := strings.Cut(line, ":")
				rawHost = strings.TrimSpace(value)
			}
		}
	}
	if hostCount != 1 {
		return nil, errors.New("restore requires one Host")
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(string(raw))))
	if err != nil {
		return nil, errors.New("restore malformed request")
	}
	req.Host = rawHost
	return req, nil
}
func (b *registryBroker) serveRestoreConnect(c net.Conn) error {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(c, restoreHeaderLimit)
	req, err := parseRestoreRequest(reader)
	if err != nil {
		return err
	}
	p := b.policy
	if b.preflight.Load() {
		p = registryPolicy{}
		for h := range b.policy {
			p[h] = true
		}
		p[restoreCanaryHost] = true
	}
	host, err := parseRegistryTarget(req.RequestURI, p)
	if err != nil || req.Method != "CONNECT" || req.Proto != "HTTP/1.1" || req.Host != req.RequestURI || req.ContentLength != 0 || len(req.TransferEncoding) > 0 || reader.Buffered() != 0 {
		return errors.New("restore CONNECT denied")
	}
	for k, vs := range req.Header {
		switch strings.ToLower(k) {
		case "user-agent", "accept":
			if len(vs) != 1 || len(vs[0]) > 1024 {
				return errors.New("restore CONNECT header denied")
			}
			for _, c := range vs[0] {
				if c < 32 || c > 126 {
					return errors.New("restore CONNECT header denied")
				}
			}
		case "proxy-connection", "connection":
			if len(vs) != 1 || !strings.EqualFold(vs[0], "keep-alive") {
				return errors.New("restore CONNECT header denied")
			}
		default:
			return errors.New("restore CONNECT header denied")
		}
	}
	if _, err = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return err
	}
	return b.inspectRestoreTLS(c, host)
}
func (b *registryBroker) inspectRestoreTLS(c net.Conn, host string) error {
	cert, ok := b.trust.certificates[host]
	if !ok {
		return errors.New("restore trust host missing")
	}
	budget := &restoreHandshakeConn{Conn: c, left: restoreHeaderLimit}
	tc := tls.Server(budget, &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello.ServerName != host {
			return nil, errors.New("restore SNI denied")
		}
		if len(hello.SupportedProtos) > 0 {
			found := false
			for _, p := range hello.SupportedProtos {
				if p == "http/1.1" {
					found = true
				}
			}
			if !found {
				return nil, errors.New("restore ALPN denied")
			}
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, Certificates: []tls.Certificate{cert}, ClientAuth: tls.NoClientCert}, nil
	}})
	if err := tc.HandshakeContext(b.ctx); err != nil {
		return err
	}
	budget.left = -1
	select {
	case b.sessions <- struct{}{}:
	default:
		return errors.New("restore concurrent session limit")
	}
	defer func() { <-b.sessions }()
	reader := bufio.NewReaderSize(tc, restoreHeaderLimit)
	for {
		_ = tc.SetDeadline(time.Now().Add(60 * time.Second))
		req, err := parseRestoreRequest(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if b.requests.Add(1) > restoreRequestLimit {
			return errors.New("restore request budget")
		}
		headers, err := validateRestoreHTTPRequest(req, host)
		if err != nil {
			return err
		}
		if host == restoreCanaryHost {
			if !b.preflight.Load() || req.RequestURI != "/canary" || req.Method != "GET" {
				return errors.New("restore preflight denied")
			}
			if _, err := io.WriteString(tc, "HTTP/1.1 200 OK\r\nContent-Length: 16\r\nConnection: close\r\n\r\nSWARM_RESTORE_OK"); err != nil {
				return err
			}
			return nil
		}
		if err := b.forward(tc, req, host, headers); err != nil {
			return err
		}
		if req.Close {
			return nil
		}
	}
}
func validateRestoreHTTPRequest(req *http.Request, host string) (http.Header, error) {
	if req.Proto != "HTTP/1.1" || (req.Method != "GET" && req.Method != "HEAD") || (req.Host != host && req.Host != host+":443") || req.ContentLength != 0 || len(req.TransferEncoding) > 0 || req.URL.IsAbs() || req.URL.Host != "" || !strings.HasPrefix(req.RequestURI, "/") || strings.HasPrefix(req.RequestURI, "//") || len(req.RequestURI) > restoreURLLimit || strings.ContainsAny(req.RequestURI, "?#\\") {
		return nil, errors.New("restore request denied")
	}
	// Encoded query/authority separators and controls are excluded too.
	path, err := url.PathUnescape(req.RequestURI)
	if err != nil || strings.ContainsAny(path, "?#\\\r\n\x00") {
		return nil, errors.New("restore path denied")
	}
	out := http.Header{}
	for k, vs := range req.Header {
		lower := strings.ToLower(k)
		switch lower {
		case "authorization", "proxy-authorization", "cookie", "upgrade", "expect", "trailer", "te", "transfer-encoding":
			return nil, errors.New("restore credential or protocol header denied")
		case "connection":
			if len(vs) != 1 || (!strings.EqualFold(vs[0], "close") && !strings.EqualFold(vs[0], "keep-alive")) {
				return nil, errors.New("restore connection header denied")
			}
		case "content-length":
			if len(vs) != 1 || vs[0] != "0" {
				return nil, errors.New("restore body denied")
			}
		case "accept", "user-agent", "range", "if-range", "if-none-match", "if-modified-since":
			if len(vs) != 1 || len(vs[0]) > 1024 {
				return nil, errors.New("restore header value denied")
			}
			for _, c := range vs[0] {
				if c < 32 || c > 126 {
					return nil, errors.New("restore header characters denied")
				}
			}
			if lower == "range" && !validRestoreRange(vs[0]) {
				return nil, errors.New("restore range denied")
			}
			if lower == "if-modified-since" {
				if _, err := http.ParseTime(vs[0]); err != nil {
					return nil, errors.New("restore date denied")
				}
			}
			if lower == "if-none-match" && !validRestoreETags(vs[0], true) {
				return nil, errors.New("restore entity tag denied")
			}
			if lower == "if-range" && strings.HasPrefix(vs[0], "\"") && !validRestoreETags(vs[0], false) {
				return nil, errors.New("restore condition denied")
			}
			if lower == "if-range" && !strings.HasPrefix(vs[0], "\"") {
				if _, err := http.ParseTime(vs[0]); err != nil {
					return nil, errors.New("restore condition denied")
				}
			}
			out[k] = append([]string(nil), vs...)
		}
	}
	return out, nil
}
func validRestoreRange(s string) bool {
	if !strings.HasPrefix(s, "bytes=") {
		return false
	}
	a, z, ok := strings.Cut(strings.TrimPrefix(s, "bytes="), "-")
	if !ok || a == "" && z == "" {
		return false
	}
	parse := func(s string) (int64, bool) {
		if s == "" {
			return 0, true
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		n, e := strconv.ParseInt(s, 10, 64)
		return n, e == nil && n >= 0
	}
	x, ok := parse(a)
	if !ok {
		return false
	}
	y, ok := parse(z)
	return ok && (a == "" || z == "" || x <= y)
}
func (b *registryBroker) forward(dst net.Conn, req *http.Request, host string, headers http.Header) error {
	ctx, cancel := context.WithTimeout(b.ctx, 60*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true, ForceAttemptHTTP2: false, MaxResponseHeaderBytes: 64 << 10, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS13, ServerName: host, RootCAs: b.roots, NextProtos: []string{"http/1.1"}}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort(host, "443") {
			return nil, errors.New("restore upstream authority denied")
		}
		ips, err := resolveRegistryAddresses(ctx, host, b.resolve)
		if err != nil {
			return nil, err
		}
		return b.dial(ctx, "tcp", net.JoinHostPort(ips[0].String(), "443"))
	}}
	defer transport.CloseIdleConnections()
	upstream, err := http.NewRequestWithContext(ctx, req.Method, "https://"+host+req.RequestURI, nil)
	if err != nil {
		return errors.New("restore upstream request failed")
	}
	upstream.Header = headers
	upstream.Host = host
	response, err := transport.RoundTrip(upstream)
	if err != nil {
		return errors.New("restore verified upstream failed")
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 407 || response.StatusCode == 101 || response.StatusCode < 200 || len(response.Header.Values("Set-Cookie")) > 0 || len(response.Header.Values("WWW-Authenticate")) > 0 || len(response.Header.Values("Proxy-Authenticate")) > 0 || response.Header.Get("Upgrade") != "" {
		return errors.New("restore upstream authentication or protocol denied")
	}
	if len(response.Trailer) > 0 || len(response.Header.Values("Trailer")) > 0 {
		return errors.New("restore upstream trailers denied")
	}
	if response.ContentLength > restoreArtifactLimit {
		return errors.New("restore artifact limit")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != 304 {
		location := response.Header.Get("Location")
		if err := b.validateRedirect(upstream.URL, location); err != nil {
			return err
		}
	}
	// Rebuild response metadata, omitting all hop-by-hop and unknown headers.
	clean := http.Header{}
	for _, k := range []string{"Content-Type", "Content-Encoding", "Content-Length", "ETag", "Last-Modified", "Content-Range", "Accept-Ranges", "Cache-Control", "Expires", "Location"} {
		if vs := response.Header.Values(k); len(vs) > 0 {
			clean[http.CanonicalHeaderKey(k)] = vs
		}
	}
	if connection := response.Header.Get("Connection"); connection != "" {
		for _, k := range strings.Split(connection, ",") {
			clean.Del(strings.TrimSpace(k))
		}
	}
	response.Header = clean
	response.Close = false
	response.Request = req
	response.Body = &restoreCountingBody{ReadCloser: response.Body, b: b, left: restoreArtifactLimit, checkEOF: func() error {
		if len(response.Trailer) > 0 {
			return errors.New("restore upstream trailers denied")
		}
		return nil
	}}
	if err := response.Write(dst); err != nil {
		return errors.New("restore upstream response framing failed")
	}
	return nil
}
func (b *registryBroker) validateRedirect(base *url.URL, location string) error {
	if location == "" || len(location) > restoreURLLimit || strings.ContainsAny(location, "\\\r\n\x00") {
		return errors.New("restore redirect denied")
	}
	u, err := url.Parse(location)
	if err != nil {
		return errors.New("restore redirect denied")
	}
	u = base.ResolveReference(u)
	if u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("restore redirect denied")
	}
	authority := u.Host
	if !strings.Contains(authority, ":") {
		authority += ":443"
	}
	if _, err := parseRegistryTarget(authority, b.policy); err != nil {
		return errors.New("restore redirect denied")
	}
	if _, err := validateRestoreHTTPRequest(&http.Request{Method: "GET", Proto: "HTTP/1.1", Host: u.Hostname(), URL: &url.URL{}, RequestURI: u.EscapedPath()}, u.Hostname()); err != nil {
		return errors.New("restore redirect denied")
	}
	return nil
}

type restoreCountingBody struct {
	io.ReadCloser
	b        *registryBroker
	left     int64
	checkEOF func() error
}

func (r *restoreCountingBody) Read(p []byte) (int, error) {
	if r.left < 0 {
		return 0, errors.New("restore byte budget")
	}
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.ReadCloser.Read(p)
	r.left -= int64(n)
	if r.left < 0 || r.b.bytes.Add(int64(n)) > restoreAggregateLimit {
		return 0, errors.New("restore byte budget")
	}
	if err == io.EOF && r.checkEOF != nil {
		if e := r.checkEOF(); e != nil {
			return n, e
		}
	}
	return n, err
}

// TLS records and the accumulated ClientHello are bounded before HTTP parsing.
type restoreHandshakeConn struct {
	net.Conn
	left int
}

func (c *restoreHandshakeConn) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, errors.New("restore ClientHello limit")
	}
	if c.left < 0 {
		return c.Conn.Read(p)
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, e := c.Conn.Read(p)
	c.left -= n
	return n, e
}

func validRestoreETags(s string, list bool) bool {
	if list && s == "*" {
		return true
	}
	count := 0
	for {
		count++
		if count > 8 {
			return false
		}
		if list {
			s = strings.TrimPrefix(s, "W/")
		}
		if !strings.HasPrefix(s, "\"") {
			return false
		}
		s = s[1:]
		end := strings.IndexByte(s, '"')
		if end < 0 {
			return false
		}
		for _, c := range s[:end] {
			if c < 0x21 || c > 0x7e {
				return false
			}
		}
		s = strings.TrimSpace(s[end+1:])
		if s == "" {
			return true
		}
		if !list || !strings.HasPrefix(s, ",") {
			return false
		}
		s = strings.TrimSpace(s[1:])
	}
}

func restoreCloseError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}
