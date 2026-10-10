package config

import (
	"os"
	"path/filepath"
	"testing"
)

// allow_local_login can come from the environment. Unset or empty must fall back to the
// default (off while Microsoft login is enabled), so the break-glass login is opt-in.
func TestAllowLocalLoginFromEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := `hostname: relay.example.com
tls: {mode: selfsigned}
graph: {tenant_id: 00000000-0000-0000-0000-000000000001, client_id: c, client_secret: s}
portal:
  microsoft_login: {enabled: true, required_roles: [Relay.Admin]}
  allow_local_login: ${TEST_ALLOW_LOCAL_LOGIN}
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", false}, {"false", false}, {"true", true}} {
		t.Setenv("TEST_ALLOW_LOCAL_LOGIN", tc.env)
		c, err := Load(path)
		if err != nil {
			t.Fatalf("env %q: %v", tc.env, err)
		}
		if got := c.Portal.LocalLoginAllowed(); got != tc.want {
			t.Errorf("env %q: LocalLoginAllowed = %v, want %v", tc.env, got, tc.want)
		}
	}
}

func TestFilesModeValidation(t *testing.T) {
	base := `hostname: relay.example.com
graph: {tenant_id: t, client_id: c, client_secret: s}
tls:
  mode: files
`
	for name, tc := range map[string]struct {
		extra string
		ok    bool
	}{
		"cert and key":         {"  cert_file: /a.pem\n  key_file: /a.key\n", true},
		"with fallback pair":   {"  cert_file: /a.pem\n  key_file: /a.key\n  fallback_cert_file: /b.pem\n  fallback_key_file: /b.key\n", true},
		"no files":             {"", false},
		"key missing":          {"  cert_file: /a.pem\n", false},
		"half a fallback pair": {"  cert_file: /a.pem\n  key_file: /a.key\n  fallback_cert_file: /b.pem\n", false},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(base+tc.extra), 0o600); err != nil {
			t.Fatal(err)
		}
		// No Cloudflare token or ACME e-mail is needed in this mode.
		if _, err := Load(path); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", name, err, tc.ok)
		}
	}
}
