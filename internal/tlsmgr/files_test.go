package tlsmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"graphrelay/internal/config"
)

// writePair writes chain (server certificate first) and the server key as PEM files.
func writePair(t *testing.T, dir, name string, chain ...*testCA) (certFile, keyFile string) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
	var certs []byte
	for _, c := range chain {
		certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})...)
	}
	der, err := x509.MarshalPKCS8PrivateKey(chain[0].key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, certs, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// leafFor issues a server certificate for the given DNS name from ca.
func leafFor(t *testing.T, ca *testCA, name string, notAfter time.Time) *testCA {
	t.Helper()
	l := issue(t, name, ca, false, "")
	// Re-issue with SANs and the requested expiry: issue() makes CA-style test certificates.
	tmpl := *l.cert
	tmpl.DNSNames = []string{name}
	tmpl.NotAfter = notAfter
	tmpl.Raw, tmpl.RawTBSCertificate = nil, nil
	der, err := x509.CreateCertificate(randReader(), &tmpl, ca.cert, &l.key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	l.cert, _ = x509.ParseCertificate(der)
	return l
}

func filesConfig(certFile, keyFile string) *config.Config {
	cfg := &config.Config{Hostname: "relay.test"}
	cfg.TLS.Mode, cfg.TLS.CertFile, cfg.TLS.KeyFile = "files", certFile, keyFile
	return cfg
}

func TestFilesModeServesOwnChain(t *testing.T) {
	root := issue(t, "Company Root CA", nil, true, "")
	inter := issue(t, "Company Issuing CA", root, true, "")
	leaf := leafFor(t, inter, "relay.test", time.Now().Add(90*24*time.Hour))
	certFile, keyFile := writePair(t, t.TempDir(), "relay", leaf, inter)

	m, err := Setup(context.Background(), filesConfig(certFile, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", m.Warnings)
	}
	// A client that trusts only the company root accepts the relay: the intermediate is sent along.
	pool := x509.NewCertPool()
	pool.AddCert(root.cert)
	conn, err := tls.Dial("tcp", serve(t, m), &tls.Config{RootCAs: pool, ServerName: "relay.test"})
	if err != nil {
		t.Fatalf("handshake with the company root as trust anchor: %v", err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().PeerCertificates; len(got) != 2 || got[0].Subject.CommonName != "relay.test" || got[1].Subject.CommonName != "Company Issuing CA" {
		t.Fatalf("served chain = %v", names(got))
	}
	if info := m.Info(); info.Err != nil || info.Issuer != "Company Issuing CA" {
		t.Fatalf("info = %+v", info)
	}
}

func TestFilesModeFallbackPair(t *testing.T) {
	dir := t.TempDir()
	ca := issue(t, "Company Root CA", nil, true, "")
	ec := leafFor(t, ca, "relay.test", time.Now().Add(24*time.Hour*90))
	ecCert, ecKey := writePair(t, dir, "ec", ec)

	rs, err := selfSigned([]string{"relay.test"}, true) // an RSA certificate for the fallback
	if err != nil {
		t.Fatal(err)
	}
	rsCert, rsKey := filepath.Join(dir, "rsa.pem"), filepath.Join(dir, "rsa.key")
	der, _ := x509.MarshalPKCS8PrivateKey(rs.PrivateKey)
	os.WriteFile(rsCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rs.Certificate[0]}), 0o644)
	os.WriteFile(rsKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)

	cfg := filesConfig(ecCert, ecKey)
	cfg.TLS.FallbackCertFile, cfg.TLS.FallbackKeyFile = rsCert, rsKey
	m, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, m)
	if got, err := keyServed(addr, &tls.Config{}); err != nil || got != x509.ECDSA {
		t.Errorf("modern client: %v %v", got, err)
	}
	if got, err := keyServed(addr, &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: rsaOnlySuites}); err != nil || got != x509.RSA {
		t.Errorf("RSA-only client: %v %v", got, err)
	}
}

func TestFilesModeStartupErrorsAndWarnings(t *testing.T) {
	dir := t.TempDir()
	ca := issue(t, "Company Root CA", nil, true, "")
	good := leafFor(t, ca, "relay.test", time.Now().Add(90*24*time.Hour))
	other := leafFor(t, ca, "relay.test", time.Now().Add(90*24*time.Hour))
	certFile, _ := writePair(t, dir, "good", good)
	_, otherKey := writePair(t, dir, "other", other)

	// A key that belongs to another certificate must stop the relay at startup.
	if _, err := Setup(context.Background(), filesConfig(certFile, otherKey)); err == nil {
		t.Error("mismatched key accepted")
	}
	if _, err := Setup(context.Background(), filesConfig(filepath.Join(dir, "missing.pem"), otherKey)); err == nil {
		t.Error("missing certificate file accepted")
	}

	// Wrong name and near expiry: the relay starts, with warnings.
	wrong := leafFor(t, ca, "other-host.test", time.Now().Add(3*24*time.Hour))
	wc, wk := writePair(t, dir, "wrong", wrong)
	m, err := Setup(context.Background(), filesConfig(wc, wk))
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(m.Warnings, "\n")
	if !strings.Contains(all, `not valid for the relay's hostname "relay.test"`) || !strings.Contains(all, "expires on") {
		t.Errorf("warnings = %q", all)
	}
}

func TestFileCertReloadsWhenFilesChange(t *testing.T) {
	dir := t.TempDir()
	ca := issue(t, "Company Root CA", nil, true, "")
	first := leafFor(t, ca, "relay.test", time.Now().Add(30*24*time.Hour))
	certFile, keyFile := writePair(t, dir, "relay", first)

	f, err := loadFileCert(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	var problems []bool
	f.notify = func(msg string, problem bool) { events, problems = append(events, msg), append(problems, problem) }
	serial := func() string {
		f.mu.Lock()
		f.checked = time.Time{} // force the next handshake to look at the files
		f.mu.Unlock()
		c, err := f.get(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Leaf.SerialNumber.String()
	}
	old := serial()
	if len(events) != 0 {
		t.Fatalf("events without a change: %v", events)
	}

	// A renewal in progress: the new certificate is in place but the key isn't yet.
	second := leafFor(t, ca, "relay.test", time.Now().Add(90*24*time.Hour))
	otherDir := t.TempDir()
	newCert, newKey := writePair(t, otherDir, "relay", second)
	copyFile(t, newCert, certFile)
	bump(t, certFile, 1)
	if got := serial(); got != old {
		t.Fatal("served a certificate whose key doesn't match")
	}
	serial() // the same broken state again
	if len(events) != 1 || !strings.Contains(events[0], "still using the previous certificate") {
		t.Fatalf("events after a half-written renewal: %v", events)
	}

	// The key arrives: the new certificate is picked up without a restart.
	copyFile(t, newKey, keyFile)
	bump(t, keyFile, 2)
	if got := serial(); got == old || got != second.cert.SerialNumber.String() {
		t.Fatal("new certificate was not loaded")
	}
	if len(events) != 2 || !strings.Contains(events[1], "reloaded") || !problems[0] || problems[1] {
		t.Fatalf("events after the renewal: %v", events)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// bump moves a file's modification time forward, so the change is visible on file systems
// with coarse timestamps.
func bump(t *testing.T, path string, minutes int) {
	t.Helper()
	when := time.Now().Add(time.Duration(minutes) * time.Minute)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
