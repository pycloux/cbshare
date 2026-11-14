package tlsutil

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memSettings map[string][]byte

func (m memSettings) Setting(_ context.Context, k string) ([]byte, bool, error) {
	v, ok := m[k]
	return v, ok, nil
}

func (m memSettings) SetSetting(_ context.Context, k string, v []byte) error {
	m[k] = v
	return nil
}

func TestSelfSignedIsReusedAcrossStarts(t *testing.T) {
	ctx := context.Background()
	s := memSettings{}
	hosts := []string{"localhost", "192.168.1.10"}

	first, err := Load(ctx, "", "", hosts, s)
	if err != nil {
		t.Fatal(err)
	}
	if !first.SelfSigned || !first.Generated {
		t.Fatalf("expected a generated self-signed certificate: %+v", first)
	}
	leaf, _ := x509.ParseCertificate(first.Certificate.Certificate[0])
	if err := leaf.VerifyHostname("192.168.1.10"); err != nil {
		t.Fatal(err)
	}

	second, err := Load(ctx, "", "", hosts, s)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generated || second.Fingerprint != first.Fingerprint {
		t.Fatal("stored certificate should be reused")
	}

	third, err := Load(ctx, "", "", []string{"localhost", "10.0.0.5"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Generated || third.Fingerprint == first.Fingerprint {
		t.Fatal("a certificate not covering the new address must be regenerated")
	}
}

func TestExpiringCertificateIsRenewed(t *testing.T) {
	s := memSettings{}
	hosts := []string{"localhost"}
	certPEM, keyPEM, err := SelfSigned(hosts, time.Now().Add(-validity+24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s[certSetting], s[keySetting] = certPEM, keyPEM
	r, err := Load(context.Background(), "", "", hosts, s)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Generated {
		t.Fatal("certificate expiring within 30 days should be renewed")
	}
}

func TestCustomCertificate(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, err := SelfSigned([]string{"example.lan"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cf, kf := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cf, certPEM, 0o600)
	os.WriteFile(kf, keyPEM, 0o600)
	s := memSettings{}
	r, err := Load(context.Background(), cf, kf, nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if r.SelfSigned || len(s) != 0 {
		t.Fatal("custom certificate must be used as is and not stored")
	}
	if _, err := Load(context.Background(), cf, filepath.Join(dir, "missing.pem"), nil, s); err == nil {
		t.Fatal("expected an error for a missing key file")
	}
}

func TestHosts(t *testing.T) {
	h := Hosts("192.168.1.10")
	found := false
	for _, x := range h {
		if x == "192.168.1.10" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bind address missing from %v", h)
	}
}
