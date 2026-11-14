// Package tlsutil provides the server's TLS certificate: either a custom
// certificate supplied by the user, or a self-signed one generated on first
// start and kept in the encrypted database so browsers see the same
// certificate across restarts.
package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// Settings persists the self-signed certificate (implemented by store.Store).
type Settings interface {
	Setting(ctx context.Context, key string) ([]byte, bool, error)
	SetSetting(ctx context.Context, key string, value []byte) error
}

// Setting keys used to persist the self-signed certificate.
const (
	certSetting = "tls_cert_pem"
	keySetting  = "tls_key_pem"
)

// validity of generated certificates, and the remaining lifetime under which
// a stored certificate is renewed.
const (
	validity    = 825 * 24 * time.Hour
	renewBefore = 30 * 24 * time.Hour
)

// Result describes the certificate in use.
type Result struct {
	Certificate tls.Certificate
	SelfSigned  bool
	Generated   bool   // a new self-signed certificate was created at this start
	Fingerprint string // SHA-256 of the leaf certificate, colon-separated hex
}

// Load returns the custom certificate when certFile and keyFile are set,
// otherwise a self-signed certificate valid for hosts, reusing the stored
// one when it still covers every host and is not about to expire.
func Load(ctx context.Context, certFile, keyFile string, hosts []string, s Settings) (Result, error) {
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return Result{}, fmt.Errorf("load TLS certificate: %w", err)
		}
		return Result{Certificate: cert, Fingerprint: fingerprint(cert.Certificate[0])}, nil
	}

	certPEM, ok1, err := s.Setting(ctx, certSetting)
	if err != nil {
		return Result{}, err
	}
	keyPEM, ok2, err := s.Setting(ctx, keySetting)
	if err != nil {
		return Result{}, err
	}
	if ok1 && ok2 {
		if cert, err := tls.X509KeyPair(certPEM, keyPEM); err == nil && covers(cert, hosts) {
			return Result{Certificate: cert, SelfSigned: true, Fingerprint: fingerprint(cert.Certificate[0])}, nil
		}
	}

	certPEM, keyPEM, err = SelfSigned(hosts, time.Now())
	if err != nil {
		return Result{}, err
	}
	if err := s.SetSetting(ctx, certSetting, certPEM); err != nil {
		return Result{}, err
	}
	if err := s.SetSetting(ctx, keySetting, keyPEM); err != nil {
		return Result{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Result{}, err
	}
	return Result{Certificate: cert, SelfSigned: true, Generated: true, Fingerprint: fingerprint(cert.Certificate[0])}, nil
}

// SelfSigned creates an ECDSA P-256 certificate and key (PEM) for hosts,
// which may be IP addresses or DNS names.
func SelfSigned(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "cpsync", Organization: []string{"cpsync (self-signed)"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Marked as its own CA so that users can import it into their trust
		// store to get rid of browser warnings.
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// Hosts returns the names and addresses a self-signed certificate should
// cover for a server bound to bind: the bind address (or every local
// address when bound to all interfaces), the hostname, and localhost.
func Hosts(bind string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts = append(hosts, name)
	}
	if ip := net.ParseIP(bind); ip != nil && !ip.IsUnspecified() {
		return append(hosts, ip.String())
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && !ipnet.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipnet.IP.String())
			}
		}
	}
	return hosts
}

// covers reports whether a stored certificate can be reused: it must be
// valid for every host and not expire within renewBefore.
func covers(cert tls.Certificate, hosts []string) bool {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || time.Until(leaf.NotAfter) < renewBefore {
		return false
	}
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// fingerprint returns the SHA-256 fingerprint in the format browsers show
// in their certificate viewer, so users can compare the two.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
