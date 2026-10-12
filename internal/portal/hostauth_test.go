package portal

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"graphrelay/internal/config"
)

func TestHostAddressList(t *testing.T) {
	e := newPortalEnv(t, config.MicrosoftLoginConfig{RequiredRoles: []string{"Relay.Admin"}})
	resp, _ := e.signIn(t, map[string]any{"roles": []string{"Relay.Admin"}})
	resp.Body.Close()
	if err := e.store.SetAllowedSenders([]string{"scanner@contoso.com"}); err != nil {
		t.Fatal(err)
	}
	_, form := e.get(t, "/hosts/new")
	v := url.Values{"name": {"cloud service"}, "sender": {"scanner@contoso.com"}, "enabled": {"on"},
		"match": {"198.51.100.11, 198.51.100.12,203.0.113.7\r\n203.0.113.9/28 198.51.100.11"}}
	if code, _ := e.post(t, form, "/hosts/save", v); code != http.StatusSeeOther {
		t.Fatalf("save = %d", code)
	}
	h, err := e.store.GetHost(1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "198.51.100.11, 198.51.100.12, 203.0.113.7, 203.0.113.0/28"; h.Match != want {
		t.Fatalf("stored match = %q, want %q", h.Match, want)
	}
	// The list comes back in the edit form and in the hosts table.
	if _, page := e.get(t, "/hosts/1"); !strings.Contains(page, ">"+h.Match+"</textarea>") {
		t.Error("list missing from the edit form")
	}
	if _, page := e.get(t, "/hosts"); !strings.Contains(page, h.Match) {
		t.Error("list missing from the hosts table")
	}
	// One bad entry rejects the whole value.
	v.Set("match", "198.51.100.11, not_an_address")
	if code, page := e.post(t, form, "/hosts/save", v); code != http.StatusUnprocessableEntity || !strings.Contains(page, "separated by commas") {
		t.Errorf("bad entry: got %d", code)
	}
}

func TestHostSMTPLoginFields(t *testing.T) {
	e := newPortalEnv(t, config.MicrosoftLoginConfig{RequiredRoles: []string{"Relay.Admin"}})
	resp, _ := e.signIn(t, map[string]any{"roles": []string{"Relay.Admin"}})
	resp.Body.Close()
	if err := e.store.SetAllowedSenders([]string{"scanner@contoso.com"}); err != nil {
		t.Fatal(err)
	}
	base := func() url.Values {
		return url.Values{"name": {"printer"}, "match": {"10.0.0.9"}, "sender": {"scanner@contoso.com"}, "enabled": {"on"}}
	}
	_, form := e.get(t, "/hosts/new")
	if !strings.Contains(form, `name="smtp_user"`) || !strings.Contains(form, `data-generate="smtp_password"`) ||
		!strings.Contains(form, `data-generate-user="smtp_user"`) {
		t.Fatal("login fields missing on host form")
	}

	for _, tc := range []struct{ user, pass, want string }{
		{"printer01", "", "Set an SMTP password"},
		{"printer01", "short", "at least 12 characters"},
		{"", "a-long-password-1", "Enter an SMTP username"},
		{"bad user!", "a-long-password-1", "may contain letters"},
	} {
		v := base()
		v.Set("smtp_user", tc.user)
		v.Set("smtp_password", tc.pass)
		code, page := e.post(t, form, "/hosts/save", v)
		if code != http.StatusUnprocessableEntity || !strings.Contains(page, tc.want) {
			t.Errorf("user %q pass %q: got %d, want error %q", tc.user, tc.pass, code, tc.want)
		}
		if strings.Contains(page, tc.pass) && tc.pass != "" {
			t.Errorf("password echoed back in the form")
		}
	}

	// Valid login: stored as a bcrypt hash, never in clear.
	v := base()
	v.Set("smtp_user", "printer01")
	v.Set("smtp_password", "a-long-password-1")
	if code, _ := e.post(t, form, "/hosts/save", v); code != http.StatusSeeOther {
		t.Fatalf("save = %d", code)
	}
	h, err := e.store.GetHost(1)
	if err != nil {
		t.Fatal(err)
	}
	if h.SMTPUser != "printer01" || bcrypt.CompareHashAndPassword([]byte(h.SMTPPassHash), []byte("a-long-password-1")) != nil {
		t.Fatalf("stored login = %q / %q", h.SMTPUser, h.SMTPPassHash)
	}
	_, list := e.get(t, "/hosts")
	if !strings.Contains(list, ">login</span>") {
		t.Error("hosts list doesn't mark the login")
	}

	// Editing with an empty password keeps the stored one.
	_, edit := e.get(t, "/hosts/1")
	if strings.Contains(edit, h.SMTPPassHash) {
		t.Fatal("password hash rendered in the form")
	}
	v = base()
	v.Set("id", "1")
	v.Set("name", "printer renamed")
	v.Set("smtp_user", "printer01")
	e.post(t, edit, "/hosts/save", v)
	if h2, _ := e.store.GetHost(1); h2.SMTPPassHash != h.SMTPPassHash || h2.Name != "printer renamed" {
		t.Fatalf("password not kept on edit: %+v", h2)
	}

	// "Remove SMTP login" clears both fields.
	v.Set("smtp_remove", "on")
	e.post(t, edit, "/hosts/save", v)
	if h3, _ := e.store.GetHost(1); h3.SMTPUser != "" || h3.SMTPPassHash != "" {
		t.Fatalf("login not removed: %+v", h3)
	}
}
