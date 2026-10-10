package config

import "testing"

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "x")
	t.Setenv("GRAPH_TENANT_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("GRAPH_CLIENT_ID", "c")
	t.Setenv("GRAPH_CLIENT_SECRET", "s")
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.Mode != "acme" || !c.TLS.RSAFallbackEnabled() || c.TLS.CertFile != "" {
		t.Fatalf("tls = %+v", c.TLS)
	}
}
