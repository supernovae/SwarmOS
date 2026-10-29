package sandbox

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" // JKS v2 integrity format requires SHA-1.
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

const restoreTrustPassword = "changeit"
const restoreCanaryHost = "preflight.swarm.invalid"

type restoreTrust struct {
	pem, jks     []byte
	certificates map[string]tls.Certificate
}

func newRestoreTrust(hosts []string) (*restoreTrust, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial := func() *big.Int {
		n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			return nil
		}
		return n
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Swarm restore ephemeral trust"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if ca.SerialNumber == nil {
		return nil, fmt.Errorf("restore trust entropy unavailable")
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	// Parse the issued CA so generated extensions (including its subject key
	// identifier) are available when signing leaves.
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	t := &restoreTrust{pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), jks: encodeTrustJKS(der, now), certificates: make(map[string]tls.Certificate)}
	for _, host := range hosts {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		leaf := &x509.Certificate{SerialNumber: serial(), DNSNames: []string{host}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
		if leaf.SerialNumber == nil {
			return nil, fmt.Errorf("restore trust entropy unavailable")
		}
		cert, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			return nil, err
		}
		t.certificates[host] = tls.Certificate{Certificate: [][]byte{cert, der}, PrivateKey: leafKey}
	}
	return t, nil
}

// encodeTrustJKS emits one public trusted-certificate entry, never a key entry.
func encodeTrustJKS(der []byte, now time.Time) []byte {
	var b bytes.Buffer
	put := func(v any) { _ = binary.Write(&b, binary.BigEndian, v) }
	utf := func(s string) { put(uint16(len(s))); b.WriteString(s) }
	put(uint32(0xfeedfeed))
	put(uint32(2))
	put(uint32(1))
	put(uint32(2))
	utf("swarm-restore")
	put(now.UnixMilli())
	utf("X.509")
	put(uint32(len(der)))
	b.Write(der)
	h := sha1.New()
	for _, c := range restoreTrustPassword {
		_, _ = h.Write([]byte{byte(c >> 8), byte(c)})
	}
	_, _ = h.Write([]byte("Mighty Aphrodite"))
	_, _ = h.Write(b.Bytes())
	return append(b.Bytes(), h.Sum(nil)...)
}
