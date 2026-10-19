package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net/smtp"
	"path/filepath"
	"strings"
	"testing"

	"graphrelay/internal/config"
	"graphrelay/internal/store"
	"graphrelay/internal/tlsmgr"
)

const authMsg = "To: x@example.com\r\nSubject: auth test\r\n\r\nbody\r\n"

func setupAuth(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{Hostname: "relay.test", DataDir: filepath.Join(t.TempDir(), "data")}
	cfg.TLS.Mode = "selfsigned"
	tm, err := tlsmgr.Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	e := setupTLS(t, tm.TLSConfig)
	hash, err := HashSMTPPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	h := &store.Host{Name: "printer", Match: "127.0.0.1", Sender: "relay@contoso.com", RewriteFrom: true,
		Enabled: true, Logins: []store.Login{{Username: "printer01", PassHash: hash}}}
	if err := e.store.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetAllowedSenders([]string{"relay@contoso.com", "payroll@contoso.com", "planning@contoso.com"}); err != nil {
		t.Fatal(err)
	}
	return e
}

// loginAuth is the client side of AUTH LOGIN (net/smtp only has PLAIN and CRAM-MD5).
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch string(fromServer) {
	case "Username:":
		return []byte(a.user), nil
	case "Password:":
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected challenge " + string(fromServer))
}

// send runs one SMTP transaction. With useTLS it does STARTTLS first; with auth it logs in.
func (e *env) send(t *testing.T, useTLS bool, auth smtp.Auth) error {
	t.Helper()
	c, err := smtp.Dial(e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		return err
	}
	if useTLS {
		if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
			return err
		}
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail("app@client.test"); err != nil {
		return err
	}
	if err := c.Rcpt("x@example.com"); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	w.Write([]byte(authMsg))
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestAuthRequiredWhenHostHasCredentials(t *testing.T) {
	e := setupAuth(t)
	wantCode(t, e.send(t, true, nil), "530")
	if e.fake.mimeUser != "" {
		t.Fatal("message was sent without login")
	}
}

func TestAuthPlainAndLogin(t *testing.T) {
	e := setupAuth(t)
	// net/smtp's PlainAuth refuses non-TLS connections to non-localhost; we always use TLS here.
	if err := e.send(t, true, smtp.PlainAuth("", "printer01", "correct horse battery", "127.0.0.1")); err != nil {
		t.Fatalf("PLAIN: %v", err)
	}
	if e.fake.mimeUser != "relay@contoso.com" {
		t.Fatalf("sent as %q", e.fake.mimeUser)
	}
	e.fake.mimeUser = ""
	if err := e.send(t, true, loginAuth{"printer01", "correct horse battery"}); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	if e.fake.mimeUser != "relay@contoso.com" {
		t.Fatalf("LOGIN: sent as %q", e.fake.mimeUser)
	}
}

func TestAuthWrongPasswordAndLockout(t *testing.T) {
	e := setupAuth(t)
	wantCode(t, e.send(t, true, loginAuth{"printer01", "wrong"}), "535")
	wantCode(t, e.send(t, true, loginAuth{"someone", "correct horse battery"}), "535")
	logs, _ := e.store.ListLog(store.LogFilter{Status: "rejected"})
	if len(logs) != 2 || !strings.Contains(logs[0].Error, "login failed") {
		t.Fatalf("failed logins not logged: %+v", logs)
	}
	for i := 0; i < authMaxFailures-2; i++ {
		e.send(t, true, loginAuth{"printer01", "wrong"})
	}
	// Locked out now, even with the right password.
	wantCode(t, e.send(t, true, loginAuth{"printer01", "correct horse battery"}), "454")
}

func TestAuthNotOfferedWithoutTLS(t *testing.T) {
	e := setupAuth(t)
	c, err := smtp.Dial(e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Hello("client.test")
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("AUTH offered on a plain-text connection")
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	if ok, mechs := c.Extension("AUTH"); !ok || !strings.Contains(mechs, "PLAIN") || !strings.Contains(mechs, "LOGIN") {
		t.Fatalf("AUTH after STARTTLS = %v %q", ok, mechs)
	}
}

func TestNoAuthHostUnchanged(t *testing.T) {
	e := setupAuth(t)
	h, _ := e.store.GetHost(1)
	h.Logins = nil
	if err := e.store.SaveHost(&h); err != nil {
		t.Fatal(err)
	}
	// Without credentials on the rule: works by IP, and AUTH isn't offered even over TLS.
	if err := e.send(t, true, nil); err != nil {
		t.Fatalf("IP-only host: %v", err)
	}
	c, _ := smtp.Dial(e.addr)
	defer c.Close()
	c.Hello("client.test")
	c.StartTLS(&tls.Config{InsecureSkipVerify: true})
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("AUTH offered to a host without credentials")
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := HashSMTPPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// One rule with several logins: each login sends as its own mailbox (or the rule's).
func TestSeveralLoginsOneHost(t *testing.T) {
	e := setupAuth(t)
	h, _ := e.store.GetHost(1)
	h.Logins = append(h.Logins,
		store.Login{Username: "payroll", PassHash: mustHash(t, "payroll password 1"), Sender: "payroll@contoso.com"},
		store.Login{Username: "planning", PassHash: mustHash(t, "planning password 1"), Sender: "planning@contoso.com"})
	if err := e.store.SaveHost(&h); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ user, pass, mailbox string }{
		{"payroll", "payroll password 1", "payroll@contoso.com"},
		{"planning", "planning password 1", "planning@contoso.com"},
		{"printer01", "correct horse battery", "relay@contoso.com"}, // no own mailbox: the rule's
	} {
		e.fake.mimeUser = ""
		if err := e.send(t, true, loginAuth{tc.user, tc.pass}); err != nil {
			t.Fatalf("%s: %v", tc.user, err)
		}
		if e.fake.mimeUser != tc.mailbox {
			t.Errorf("%s sent as %q, want %q", tc.user, e.fake.mimeUser, tc.mailbox)
		}
	}
	// Another login's password doesn't work for this username.
	wantCode(t, e.send(t, true, loginAuth{"payroll", "planning password 1"}), "535")
	// Still no anonymous sending.
	wantCode(t, e.send(t, true, nil), "530")

	logs, _ := e.store.ListLog(store.LogFilter{Status: "sent"})
	if len(logs) != 3 || logs[0].Sender != "relay@contoso.com" || logs[2].Sender != "payroll@contoso.com" {
		t.Fatalf("log senders: %+v", logs)
	}
}

// Two rules for the same address, told apart by the login (the setup that did not work before).
func TestTwoRulesSameAddressByLogin(t *testing.T) {
	e := setupAuth(t)
	other := &store.Host{Name: "planning app", Match: "127.0.0.1/32", Sender: "planning@contoso.com", RewriteFrom: true,
		Enabled: true, Logins: []store.Login{{Username: "planner", PassHash: mustHash(t, "planner password")}}}
	if err := e.store.SaveHost(other); err != nil {
		t.Fatal(err)
	}
	// "printer" (exact IP) is the more specific rule, but the login of the /32 rule picks that one.
	if err := e.send(t, true, loginAuth{"planner", "planner password"}); err != nil {
		t.Fatal(err)
	}
	if e.fake.mimeUser != "planning@contoso.com" {
		t.Fatalf("sent as %q", e.fake.mimeUser)
	}
	if err := e.send(t, true, loginAuth{"printer01", "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	if e.fake.mimeUser != "relay@contoso.com" {
		t.Fatalf("sent as %q", e.fake.mimeUser)
	}
}

// A login removed in the portal stops working for a connection that logged in with it earlier.
func TestRemovedLoginNoLongerSends(t *testing.T) {
	e := setupAuth(t)
	c, err := smtp.Dial(e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Hello("client.test")
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Auth(loginAuth{"printer01", "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	h, _ := e.store.GetHost(1)
	h.Logins = []store.Login{{Username: "printer02", PassHash: mustHash(t, "another password")}}
	if err := e.store.SaveHost(&h); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c.Mail("app@client.test"), "530")
}
