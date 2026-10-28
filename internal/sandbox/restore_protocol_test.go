package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const restoreFixtureHost = "registry.example"
const restoreFixtureCDN = "cdn.example"

type restoreFixture struct {
	listener *restoreTestListener
	broker   *registryBroker
	roots    *x509.CertPool
	proxy    *url.URL
	upstream *httptest.Server
	dials    atomic.Int64
	mu       sync.Mutex
	seen     []*http.Request
}

func newRestoreFixture(t *testing.T, handler http.HandlerFunc) *restoreFixture {
	t.Helper()
	trust, err := newRestoreTrust([]string{restoreFixtureHost, restoreFixtureCDN, restoreCanaryHost})
	if err != nil {
		t.Fatal(err)
	}
	f := &restoreFixture{roots: x509.NewCertPool()}
	f.roots.AppendCertsFromPEM(trust.pem)
	// A separate CA signs upstream certs. Interception trust is never upstream trust.
	upstreamTrust, err := newRestoreTrust([]string{restoreFixtureHost, restoreFixtureCDN})
	if err != nil {
		t.Fatal(err)
	}
	upstreamRoots := x509.NewCertPool()
	upstreamRoots.AppendCertsFromPEM(upstreamTrust.pem)
	upstreamListener := newRestoreTestListener()
	f.upstream = &httptest.Server{Listener: upstreamListener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != r.Host {
			t.Errorf("SNI %q != Host %q", r.TLS.ServerName, r.Host)
		}
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		f.mu.Unlock()
		if handler != nil {
			handler(w, r)
		} else {
			w.Header().Set("ETag", "\"fixture\"")
			_, _ = io.WriteString(w, "artifact")
		}
	})}}
	f.upstream.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		c, ok := upstreamTrust.certificates[h.ServerName]
		if !ok {
			return nil, fmt.Errorf("unexpected name")
		}
		return &c, nil
	}}
	f.upstream.StartTLS()
	t.Cleanup(f.upstream.Close)
	l := newRestoreTestListener()
	f.listener = l
	f.broker = newRegistryBroker(context.Background(), l, registryPolicy{restoreFixtureHost: true, restoreFixtureCDN: true}, trust)
	f.broker.roots = upstreamRoots
	f.broker.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	f.broker.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		f.dials.Add(1)
		if address != "8.8.8.8:443" {
			t.Errorf("not numeric pinned dial: %q", address)
		}
		return upstreamListener.dial(ctx)
	}
	f.proxy = &url.URL{Scheme: "http", Host: l.Addr().String()}
	f.broker.serve()
	t.Cleanup(func() { _ = f.broker.close() })
	return f
}
func (f *restoreFixture) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return f.listener.dial(ctx) }, Proxy: http.ProxyURL(f.proxy), TLSClientConfig: &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS12}, DisableCompression: true}, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func fixtureRead(t *testing.T, c *http.Client, method, target string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}
func (f *restoreFixture) raw(t *testing.T, connect, sni string, version uint16, alpn []string) *tls.Conn {
	t.Helper()
	c, err := f.listener.dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nUser-Agent: Native-Java-Maven-Gradle-NuGet-fixture\r\nAccept: text/html, application/json, */*\r\nProxy-Connection: keep-alive\r\n\r\n", connect, connect)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", r, err)
	}
	return tls.Client(c, &tls.Config{RootCAs: f.roots, ServerName: sni, MinVersion: version, MaxVersion: version, NextProtos: alpn})
}
func TestRestoreProtocolNativeBootstrap(t *testing.T) {
	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, alpn := range [][]string{nil, {"http/1.1"}} {
			t.Run(fmt.Sprint(v, alpn), func(t *testing.T) {
				f := newRestoreFixture(t, nil)
				c := f.raw(t, restoreFixtureHost, restoreFixtureHost, v, alpn)
				if f.dials.Load() != 0 {
					t.Fatal("CONNECT dialed upstream")
				}
				if err := c.Handshake(); err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(c, "GET /pkg.jar HTTP/1.1\r\nHost: registry.example\r\nConnection: close\r\n\r\n")
				r, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil || string(data) != "artifact" || r.Header.Get("ETag") != "\"fixture\"" {
					t.Fatalf("response %q %v %v", data, r.Header, err)
				}
				if f.dials.Load() != 1 {
					t.Fatal("expected one reconstructed HTTPS exchange")
				}
			})
		}
	}
}
func TestRestoreProtocolRegistryShapes(t *testing.T) {
	packageBytes := []byte{0x50, 0x4b, 3, 4, 0, 255, 1, 2}
	paths := map[string]string{
		"/v3/index.json":                      `{"resources":[{"@id":"https://cdn.example/flat/","@type":"PackageBaseAddress/3.0.0"},{"@id":"https://registry.example/registration/pkg/index.json","@type":"RegistrationsBaseUrl"}]}`,
		"/registration/pkg/index.json":        `{"items":[{"catalogEntry":{"version":"1.0.0"}}]}`,
		"/flat/pkg/index.json":                `{"versions":["1.0.0"]}`,
		"/org/example/pkg/1/pkg-1.pom":        "<project><version>1</version></project>",
		"/org/example/pkg/maven-metadata.xml": "<metadata><versioning><latest>1</latest></versioning></metadata>",
		"/org/example/pkg/1/pkg-1.jar.sha1":   "0123456789abcdef0123456789abcdef01234567",
	}
	f := newRestoreFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Native-Benign") != "" {
			t.Error("unapproved header forwarded")
		}
		if s, ok := paths[r.URL.Path]; ok {
			_, _ = io.WriteString(w, s)
			return
		}
		switch r.URL.Path {
		case "/flat/pkg/1.0.0/pkg.1.0.0.nupkg", "/org/example/pkg/1/pkg-1.jar":
			w.Header().Set("ETag", "\"v1\"")
			_, _ = w.Write(packageBytes)
		case "/relative":
			http.Redirect(w, r, "/org/example/pkg/1/pkg-1.pom", http.StatusFound)
		case "/cdn":
			http.Redirect(w, r, "https://cdn.example/flat/pkg/1.0.0/pkg.1.0.0.nupkg", http.StatusFound)
		case "/gradle.jar":
			w.Header().Set("ETag", "\"v1\"")
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			if r.Header.Get("If-None-Match") == "\"v1\"" {
				w.WriteHeader(304)
				return
			}
			if r.Header.Get("Range") == "bytes=1-3" {
				w.Header().Set("Content-Range", "bytes 1-3/8")
				w.WriteHeader(206)
				_, _ = w.Write(packageBytes[1:4])
				return
			}
			w.Header().Set("Content-Length", "8")
			if r.Method != "HEAD" {
				_, _ = w.Write(packageBytes)
			}
		case "/chunked":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			_, _ = w.Write(packageBytes[:4])
			w.(http.Flusher).Flush()
			_, _ = w.Write(packageBytes[4:])
		default:
			http.NotFound(w, r)
		}
	})
	client := f.client()
	defer client.CloseIdleConnections()
	base := "https://" + restoreFixtureHost
	for path, expected := range paths {
		_, b := fixtureRead(t, client, "GET", base+path, http.Header{"X-Native-Benign": {"drop me"}, "Accept": {"application/json"}, "User-Agent": {"NuGet Maven Gradle fixture"}})
		if string(b) != expected {
			t.Fatalf("%s bytes changed", path)
		}
	}
	for _, target := range []string{base + "/org/example/pkg/1/pkg-1.jar", "https://cdn.example/flat/pkg/1.0.0/pkg.1.0.0.nupkg", base + "/chunked"} {
		_, b := fixtureRead(t, client, "GET", target, nil)
		if !bytes.Equal(b, packageBytes) {
			t.Fatal("package bytes changed")
		}
	}
	for _, path := range []string{"/relative", "/cdn"} {
		r, _ := fixtureRead(t, client, "GET", base+path, nil)
		if r.StatusCode != 302 {
			t.Fatal("redirect not returned")
		}
		u, _ := url.Parse(base + path)
		ref, _ := url.Parse(r.Header.Get("Location"))
		_, _ = fixtureRead(t, client, "GET", u.ResolveReference(ref).String(), nil)
	}
	r, b := fixtureRead(t, client, "HEAD", base+"/gradle.jar", nil)
	if len(b) != 0 || r.ContentLength != 8 {
		t.Fatal("HEAD framing")
	}
	r, b = fixtureRead(t, client, "GET", base+"/gradle.jar", http.Header{"Range": {"bytes=1-3"}, "If-Range": {"\"v1\""}})
	if r.StatusCode != 206 || !bytes.Equal(b, packageBytes[1:4]) {
		t.Fatal("range response")
	}
	r, _ = fixtureRead(t, client, "GET", base+"/gradle.jar", http.Header{"If-None-Match": {"\"v1\""}})
	if r.StatusCode != 304 {
		t.Fatal("conditional response")
	}
	r, _ = fixtureRead(t, client, "GET", base+"/missing", nil)
	if r.StatusCode != 404 {
		t.Fatal("normal miss rejected")
	}
}
func TestRestoreProtocolTrustBootstrap(t *testing.T) {
	a, err := newRestoreTrust([]string{restoreFixtureHost})
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRestoreTrust([]string{restoreFixtureHost})
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(a.pem)
	if block == nil || len(rest) != 0 || block.Type != "CERTIFICATE" || bytes.Contains(a.pem, []byte("PRIVATE")) {
		t.Fatal("public PEM shape")
	}
	if bytes.Equal(a.pem, b.pem) {
		t.Fatal("per-run CA reused")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(a.pem)
	cert, err := x509.ParseCertificate(a.certificates[restoreFixtureHost].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: restoreFixtureHost}); err != nil {
		t.Fatal(err)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || cert.NotAfter.Sub(cert.NotBefore) > 11*time.Minute {
		t.Fatal("leaf scope")
	}
	// Independent byte parser for JKS: assert exact format and sole trusted entry.
	data := a.jks
	if len(data) < 20 {
		t.Fatal("short JKS")
	}
	payload, checksum := data[:len(data)-20], data[len(data)-20:]
	integrity := sha1.New()
	_, _ = integrity.Write([]byte{0, 'c', 0, 'h', 0, 'a', 0, 'n', 0, 'g', 0, 'e', 0, 'i', 0, 't'})
	_, _ = integrity.Write([]byte("Mighty Aphrodite"))
	_, _ = integrity.Write(payload)
	if !bytes.Equal(checksum, integrity.Sum(nil)) {
		t.Fatal("JKS integrity")
	}
	r := bytes.NewReader(payload)
	read32 := func() uint32 {
		var v uint32
		if err := binary.Read(r, binary.BigEndian, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	readUTF := func() string {
		var n uint16
		_ = binary.Read(r, binary.BigEndian, &n)
		v := make([]byte, n)
		if _, err := io.ReadFull(r, v); err != nil {
			t.Fatal(err)
		}
		return string(v)
	}
	if read32() != 0xfeedfeed || read32() != 2 || read32() != 1 || read32() != 2 || readUTF() != "swarm-restore" {
		t.Fatal("JKS header/entry")
	}
	var timestamp int64
	_ = binary.Read(r, binary.BigEndian, &timestamp)
	if timestamp <= 0 || readUTF() != "X.509" {
		t.Fatal("JKS timestamp/type")
	}
	der := make([]byte, read32())
	_, err = io.ReadFull(r, der)
	if err != nil || !bytes.Equal(der, block.Bytes) || r.Len() != 0 {
		t.Fatal("JKS certificate")
	}
	// Fixed known-format empty certificate entry fixture, independent of random CA.
	fixture := []byte{0xfe, 0xed, 0xfe, 0xed, 0, 0, 0, 2, 0, 0, 0, 1, 0, 0, 0, 2, 0, 13, 's', 'w', 'a', 'r', 'm', '-', 'r', 'e', 's', 't', 'o', 'r', 'e', 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 'X', '.', '5', '0', '9', 0, 0, 0, 1, 0x42}
	encoded := encodeTrustJKS([]byte{0x42}, time.UnixMilli(0))
	if !bytes.Equal(encoded[:len(encoded)-20], fixture) {
		t.Fatal("JKS known-format fixture")
	}
}
func TestRestoreProtocolVirtualHosts(t *testing.T) {
	for _, sni := range []string{"", restoreFixtureCDN, "other.example"} {
		t.Run("SNI/"+sni, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			c := f.raw(t, restoreFixtureHost, sni, tls.VersionTLS13, nil)
			if err := c.Handshake(); err == nil {
				t.Fatal("SNI accepted")
			}
			if f.dials.Load() != 0 {
				t.Fatal("SNI mismatch dialed")
			}
		})
	}
	t.Run("h2-only", func(t *testing.T) {
		f := newRestoreFixture(t, nil)
		c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, []string{"h2"})
		if err := c.Handshake(); err == nil {
			t.Fatal("h2 accepted")
		}
	})
	for _, request := range []string{
		"GET / HTTP/1.1\r\nHost: cdn.example\r\n\r\n",
		"GET https://cdn.example/ HTTP/1.1\r\nHost: registry.example\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: registry.example\r\nHost: registry.example\r\n\r\n",
		"CONNECT cdn.example:443 HTTP/1.1\r\nHost: cdn.example:443\r\n\r\n",
	} {
		t.Run(request, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, nil)
			if err := c.Handshake(); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(c, request)
			_, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err == nil {
				t.Fatal("authority escape accepted")
			}
			if f.dials.Load() != 0 {
				t.Fatal("escape dialed upstream")
			}
		})
	}
	t.Run("keepalive authority change", func(t *testing.T) {
		f := newRestoreFixture(t, nil)
		c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, nil)
		if err := c.Handshake(); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(c)
		_, _ = io.WriteString(c, "GET /a HTTP/1.1\r\nHost: registry.example\r\n\r\n")
		r, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		_, _ = io.WriteString(c, "GET /b HTTP/1.1\r\nHost: cdn.example\r\n\r\n")
		if _, err = http.ReadResponse(reader, nil); err == nil {
			t.Fatal("second Host accepted")
		}
		if f.dials.Load() != 1 {
			t.Fatal("second host dialed")
		}
	})
}
func TestRestoreProtocolCredentialsAndFraming(t *testing.T) {
	for _, header := range []string{"Authorization: Bearer SYNTHETIC", "Authorization: Basic SYNTHETIC", "Proxy-Authorization: Basic SYNTHETIC", "Cookie: SYNTHETIC", "Upgrade: websocket", "Expect: 100-continue", "Transfer-Encoding: chunked", "Content-Length: 1", "Content-Length: 0\r\nContent-Length: 0", "Transfer-Encoding: chunked\r\nContent-Length: 0", "X: one\r\n folded", "Range: bytes=0-1,3-4", "If-Modified-Since: nonsense"} {
		t.Run(header, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, nil)
			if err := c.Handshake(); err != nil {
				t.Fatal(err)
			}
			_, _ = fmt.Fprintf(c, "GET /artifact HTTP/1.1\r\nHost: registry.example\r\n%s\r\n\r\n", header)
			if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
				t.Fatal("forbidden request accepted")
			}
			if f.dials.Load() != 0 {
				t.Fatal("credential/framing request dialed upstream")
			}
		})
	}
	for _, path := range []string{"/pkg?sig=SYNTHETIC", "/pkg%3fsig=SYNTHETIC", "//cdn.example/pkg", "/pkg#fragment", "/pkg%00", "/pkg\\x"} {
		t.Run(path, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, nil)
			if err := c.Handshake(); err != nil {
				t.Fatal(err)
			}
			_, _ = fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: registry.example\r\n\r\n", path)
			if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
				t.Fatal("unsafe path accepted")
			}
			if f.dials.Load() != 0 {
				t.Fatal("unsafe path dialed")
			}
		})
	}
}
func TestRestoreProtocolResponses(t *testing.T) {
	for _, test := range []struct {
		name, header, value string
		status              int
	}{
		{"401", "", "", 401}, {"407", "", "", 407}, {"set-cookie", "Set-Cookie", "SYNTHETIC", 200}, {"auth", "WWW-Authenticate", "Basic", 200}, {"upgrade", "Upgrade", "websocket", 200},
		{"http redirect", "Location", "http://registry.example/a", 302}, {"other host", "Location", "https://other.example/a", 302}, {"userinfo", "Location", "https://u:p@registry.example/a", 302}, {"query", "Location", "https://registry.example/a?sig=x", 302}, {"missing redirect", "", "", 302}, {"oversize", "Content-Length", "268435457", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRestoreFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if test.header != "" {
					w.Header().Set(test.header, test.value)
				}
				w.WriteHeader(test.status)
			})
			c := f.client()
			defer c.CloseIdleConnections()
			r, err := c.Get("https://registry.example/a")
			if err == nil {
				_ = r.Body.Close()
				t.Fatal("forbidden response returned")
			}
		})
	}
}
func TestRestoreProtocolUpstreamTrustSeparation(t *testing.T) {
	for _, kind := range []string{"untrusted", "wrong-name", "expired", "interception-ca"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t, nil)
			switch kind {
			case "untrusted":
				f.broker.roots = x509.NewCertPool()
			case "interception-ca":
				f.broker.roots = f.roots
			case "wrong-name":
				wrong, _ := newRestoreTrust([]string{"other.example"})
				f.upstream.TLS.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					c := wrong.certificates["other.example"]
					return &c, nil
				}
				f.broker.roots = x509.NewCertPool()
				f.broker.roots.AppendCertsFromPEM(wrong.pem)
			case "expired":
				key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: restoreFixtureHost}, DNSNames: []string{restoreFixtureHost}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(-time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
				der, _ := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
				f.broker.roots = x509.NewCertPool()
				cert, _ := x509.ParseCertificate(der)
				f.broker.roots.AddCert(cert)
				f.upstream.TLS.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
				}
			}
			client := f.client()
			defer client.CloseIdleConnections()
			r, err := client.Get("https://registry.example/a")
			if err == nil {
				_ = r.Body.Close()
				t.Fatal("upstream trust failure accepted")
			}
		})
	}
}
func TestRestoreProtocolBounds(t *testing.T) {
	t.Run("headers", func(t *testing.T) {
		f := newRestoreFixture(t, nil)
		c := f.raw(t, restoreFixtureHost, restoreFixtureHost, tls.VersionTLS13, nil)
		if err := c.Handshake(); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: registry.example\r\nX: %s\r\n\r\n", strings.Repeat("x", restoreHeaderLimit))
		if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
			t.Fatal("oversize header accepted")
		}
		if f.dials.Load() != 0 {
			t.Fatal("oversize request dialed")
		}
	})
	t.Run("request budget", func(t *testing.T) {
		f := newRestoreFixture(t, nil)
		f.broker.requests.Store(restoreRequestLimit)
		r, err := f.client().Get("https://registry.example/a")
		if err == nil {
			_ = r.Body.Close()
			t.Fatal("request budget bypass")
		}
		if f.dials.Load() != 0 {
			t.Fatal("exhausted request dialed")
		}
	})
	t.Run("aggregate bytes", func(t *testing.T) {
		f := newRestoreFixture(t, nil)
		f.broker.bytes.Store(restoreAggregateLimit)
		client := f.client()
		defer client.CloseIdleConnections()
		r, err := client.Get("https://registry.example/a")
		if err == nil {
			_, err = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		if err == nil {
			t.Fatal("byte budget bypass")
		}
	})
}

// Socket-independent listener preserves real CONNECT and both TLS transcripts
// under sandboxes that deny even loopback sockets. No parser/fetch API is mocked.
type restoreTestListener struct {
	pending chan net.Conn
	closed  chan struct{}
	once    sync.Once
}

func newRestoreTestListener() *restoreTestListener {
	return &restoreTestListener{pending: make(chan net.Conn), closed: make(chan struct{})}
}
func (l *restoreTestListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.pending:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *restoreTestListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (l *restoreTestListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
}
func (l *restoreTestListener) dial(ctx context.Context) (net.Conn, error) {
	a, b, err := newRestoreTestPipe()
	if err != nil {
		return nil, err
	}
	select {
	case l.pending <- a:
		return b, nil
	case <-l.closed:
		_ = a.Close()
		_ = b.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = a.Close()
		_ = b.Close()
		return nil, ctx.Err()
	}
}

// OS pipes supply bounded stream buffering (including simultaneous TLS alerts)
// without requiring AF_INET or AF_UNIX socket permissions.
type restoreTestPipe struct{ r, w *os.File }

func newRestoreTestPipe() (net.Conn, net.Conn, error) {
	ar, bw, e := os.Pipe()
	if e != nil {
		return nil, nil, e
	}
	br, aw, e := os.Pipe()
	if e != nil {
		ar.Close()
		bw.Close()
		return nil, nil, e
	}
	return &restoreTestPipe{ar, aw}, &restoreTestPipe{br, bw}, nil
}
func (c *restoreTestPipe) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *restoreTestPipe) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *restoreTestPipe) Close() error                { return errors.Join(c.r.Close(), c.w.Close()) }
func (c *restoreTestPipe) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
}
func (c *restoreTestPipe) RemoteAddr() net.Addr { return c.LocalAddr() }
func (c *restoreTestPipe) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}
func (c *restoreTestPipe) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *restoreTestPipe) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

func TestRestoreProtocolNativeResourceDiscovery(t *testing.T) {
	for _, approveCDN := range []bool{true, false} {
		t.Run(fmt.Sprint(approveCDN), func(t *testing.T) {
			f := newRestoreFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v3/index.json" {
					_, _ = io.WriteString(w, `{"resources":[{"@id":"https://cdn.example/flat/","@type":"PackageBaseAddress/3.0.0"}]}`)
					return
				}
				if r.URL.Path == "/flat/pkg/index.json" {
					_, _ = io.WriteString(w, `{"versions":["1.0.0"]}`)
					return
				}
				_, _ = w.Write([]byte{0x50, 0x4b, 0x03, 0x04, 0, 255})
			})
			if !approveCDN {
				delete(f.broker.policy, restoreFixtureCDN)
			}
			client := f.client()
			defer client.CloseIdleConnections()
			_, index := fixtureRead(t, client, "GET", "https://registry.example/v3/index.json", nil)
			var document struct {
				Resources []struct {
					ID string `json:"@id"`
				}
			}
			if err := json.Unmarshal(index, &document); err != nil {
				t.Fatal(err)
			}
			resource := document.Resources[0].ID
			request, err := client.Get(resource + "pkg/index.json")
			if !approveCDN {
				if err == nil {
					request.Body.Close()
					t.Fatal("missing CDN policy accepted")
				}
				if f.dials.Load() != 1 {
					t.Fatal("unapproved CDN dialed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			versions, err := io.ReadAll(request.Body)
			request.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			var list struct{ Versions []string }
			if err = json.Unmarshal(versions, &list); err != nil {
				t.Fatal(err)
			}
			_, artifact := fixtureRead(t, client, "GET", resource+"pkg/"+list.Versions[0]+"/pkg."+list.Versions[0]+".nupkg", nil)
			if !bytes.Equal(artifact, []byte{0x50, 0x4b, 0x03, 0x04, 0, 255}) {
				t.Fatal("resource bytes changed")
			}
		})
	}
}
func TestRestoreProtocolContentEncoding(t *testing.T) {
	encoded := []byte{31, 139, 8, 0, 0, 255}
	f := newRestoreFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(encoded)
	})
	c := f.client()
	defer c.CloseIdleConnections()
	r, data := fixtureRead(t, c, "GET", "https://registry.example/artifact", nil)
	if r.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(data, encoded) {
		t.Fatal("encoded bytes/metadata changed")
	}
}
