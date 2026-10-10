package tlsmgr

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// fileCert is a certificate and key loaded from files ("files" mode: the operator's own CA or
// any other source). The files are re-read when they change, so a renewal that replaces them is
// picked up without a restart. A failed reload keeps the certificate already in use.
type fileCert struct {
	certFile, keyFile string
	notify            func(msg string, problem bool) // reports reloads and reload failures; may be nil

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   time.Time // newest modification time of the two files when cert was loaded
	checked time.Time
	failed  time.Time // modification time of files that failed to load, to report once
}

// recheckEvery limits how often the files are stat'ed; handshakes in between use the cached certificate.
const recheckEvery = 30 * time.Second

func loadFileCert(certFile, keyFile string) (*fileCert, error) {
	f := &fileCert{certFile: certFile, keyFile: keyFile}
	c, stamp, err := f.load()
	if err != nil {
		return nil, err
	}
	f.cert, f.stamp, f.checked = c, stamp, time.Now()
	return f, nil
}

// load reads and parses the pair. The certificate file may hold a chain (server certificate
// first, then intermediates); the key must belong to the first certificate.
func (f *fileCert) load() (*tls.Certificate, time.Time, error) {
	stamp, err := newestModTime(f.certFile, f.keyFile)
	if err != nil {
		return nil, stamp, err
	}
	c, err := tls.LoadX509KeyPair(f.certFile, f.keyFile)
	if err != nil {
		return nil, stamp, fmt.Errorf("certificate %s with key %s: %w", f.certFile, f.keyFile, err)
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return nil, stamp, err
		}
	}
	return &c, stamp, nil
}

func newestModTime(paths ...string) (time.Time, error) {
	var newest time.Time
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return newest, err
		}
		if st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	return newest, nil
}

// get is the certificate source for handshakes.
func (f *fileCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.checked) >= recheckEvery {
		f.checked = time.Now()
		f.reloadIfChanged()
	}
	return f.cert, nil
}

// reloadIfChanged swaps in new files when their modification time changed. Caller holds mu.
func (f *fileCert) reloadIfChanged() {
	stamp, err := newestModTime(f.certFile, f.keyFile)
	if err == nil && stamp.Equal(f.stamp) {
		return
	}
	c, stamp, err := f.load()
	if err != nil {
		// A renewal may be half-written (certificate replaced, key not yet). Keep the current
		// certificate and try again at the next check; report each broken state once.
		if f.notify != nil && !stamp.Equal(f.failed) {
			f.notify(fmt.Sprintf("TLS certificate files changed but could not be loaded, still using the previous certificate: %v", err), true)
		}
		f.failed = stamp
		return
	}
	f.cert, f.stamp, f.failed = c, stamp, time.Time{}
	if f.notify != nil {
		f.notify(fmt.Sprintf("TLS certificate reloaded from %s (valid until %s)", f.certFile, c.Leaf.NotAfter.Format("2006-01-02")), false)
	}
}

// certWarnings returns problems with a loaded certificate that don't stop the relay from
// starting but will stop clients from trusting it.
func certWarnings(c *x509.Certificate, file, hostname string) []string {
	var w []string
	if err := c.VerifyHostname(hostname); err != nil {
		w = append(w, fmt.Sprintf("certificate %s is not valid for the relay's hostname %q: clients that check the name will refuse it", file, hostname))
	}
	switch left := time.Until(c.NotAfter); {
	case left <= 0:
		w = append(w, fmt.Sprintf("certificate %s expired on %s", file, c.NotAfter.Format("2006-01-02")))
	case left < 14*24*time.Hour:
		w = append(w, fmt.Sprintf("certificate %s expires on %s", file, c.NotAfter.Format("2006-01-02")))
	}
	if time.Now().Before(c.NotBefore) {
		w = append(w, fmt.Sprintf("certificate %s is not valid before %s", file, c.NotBefore.Format("2006-01-02")))
	}
	return w
}
