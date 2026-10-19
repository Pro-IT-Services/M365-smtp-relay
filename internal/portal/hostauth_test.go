package portal

import (
	"fmt"
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

func TestHostSMTPLogins(t *testing.T) {
	e := newPortalEnv(t, config.MicrosoftLoginConfig{RequiredRoles: []string{"Relay.Admin"}})
	resp, _ := e.signIn(t, map[string]any{"roles": []string{"Relay.Admin"}})
	resp.Body.Close()
	if err := e.store.SetAllowedSenders([]string{"scanner@contoso.com", "payroll@contoso.com"}); err != nil {
		t.Fatal(err)
	}
	// form builds a host form submission with the given login lines: id, user, password, mailbox.
	form := func(id string, lines ...[4]string) url.Values {
		v := url.Values{"id": {id}, "name": {"printer"}, "match": {"10.0.0.9"}, "sender": {"scanner@contoso.com"}, "enabled": {"on"}}
		for _, l := range lines {
			v.Add("login_id", l[0])
			v.Add("login_user", l[1])
			v.Add("login_pass", l[2])
			v.Add("login_sender", l[3])
		}
		return v
	}
	_, page := e.get(t, "/hosts/new")
	for _, want := range []string{`name="login_user"`, `name="login_pass"`, `name="login_sender"`,
		`data-generate="row"`, `data-generate-user="row"`, `data-add-login`} {
		if !strings.Contains(page, want) {
			t.Fatalf("host form lacks %s", want)
		}
	}

	for _, tc := range []struct {
		lines [][4]string
		want  string
	}{
		{[][4]string{{"0", "printer01", "", ""}}, "Set a password for the new login"},
		{[][4]string{{"0", "printer01", "short", ""}}, "at least 12 characters"},
		{[][4]string{{"0", "bad user!", "a-long-password-1", ""}}, "may contain letters"},
		{[][4]string{{"0", "a", "a-long-password-1", ""}, {"0", "A", "a-long-password-2", ""}}, "used twice"},
		{[][4]string{{"0", "printer01", "a-long-password-1", "other@contoso.com"}}, "mailbox from the list"},
		{[][4]string{{"7", "ghost", "", ""}}, "no longer exists"},
	} {
		code, out := e.post(t, page, "/hosts/save", form("0", tc.lines...))
		if code != http.StatusUnprocessableEntity || !strings.Contains(out, tc.want) {
			t.Errorf("%v: got %d, want error %q", tc.lines, code, tc.want)
		}
		if strings.Contains(out, "a-long-password") {
			t.Errorf("%v: password echoed back in the form", tc.lines)
		}
	}
	if hosts, _ := e.store.ListHosts(); len(hosts) != 0 {
		t.Fatalf("a failed form saved a host: %+v", hosts)
	}

	// Two logins, one with its own mailbox, plus an empty line (ignored). Stored as bcrypt hashes.
	code, _ := e.post(t, page, "/hosts/save", form("0",
		[4]string{"0", "printer01", "a-long-password-1", ""},
		[4]string{"0", "payroll", "a-long-password-2", "payroll@contoso.com"},
		[4]string{"0", "", "", ""}))
	if code != http.StatusSeeOther {
		t.Fatalf("save = %d", code)
	}
	h, err := e.store.GetHost(1)
	if err != nil || len(h.Logins) != 2 {
		t.Fatalf("stored: %+v %v", h, err)
	}
	payroll, printer := h.Logins[0], h.Logins[1] // sorted by username
	if printer.Username != "printer01" || printer.Sender != "" ||
		bcrypt.CompareHashAndPassword([]byte(printer.PassHash), []byte("a-long-password-1")) != nil ||
		payroll.Username != "payroll" || payroll.Sender != "payroll@contoso.com" {
		t.Fatalf("stored logins: %+v", h.Logins)
	}
	if _, list := e.get(t, "/hosts"); !strings.Contains(list, ">2 logins</span>") {
		t.Error("hosts list doesn't show the logins")
	}

	// Edit: hashes never appear in the page; an empty password keeps the stored one; a removed
	// login is deleted; a new one is added.
	_, edit := e.get(t, "/hosts/1")
	if strings.Contains(edit, printer.PassHash) || strings.Contains(edit, payroll.PassHash) {
		t.Fatal("password hash rendered in the form")
	}
	if !strings.Contains(edit, `name="login_remove" value="`) || !strings.Contains(edit, "(empty = keep)") {
		t.Fatal("edit form lacks remove / keep-password controls for existing logins")
	}
	v := form("1",
		[4]string{fmt.Sprint(printer.ID), "printer01", "", ""},
		[4]string{fmt.Sprint(payroll.ID), "payroll", "", "payroll@contoso.com"},
		[4]string{"0", "reports", "a-long-password-3", "payroll@contoso.com"})
	v.Set("name", "printer renamed")
	v.Add("login_remove", fmt.Sprint(payroll.ID))
	if code, _ := e.post(t, edit, "/hosts/save", v); code != http.StatusSeeOther {
		t.Fatalf("edit = %d", code)
	}
	h2, _ := e.store.GetHost(1)
	if h2.Name != "printer renamed" || len(h2.Logins) != 2 || h2.Logins[0].Username != "printer01" ||
		h2.Logins[0].PassHash != printer.PassHash || h2.Logins[1].Username != "reports" {
		t.Fatalf("after edit: %+v", h2)
	}

	// Removing every login makes the host address-only again.
	v = form("1", [4]string{fmt.Sprint(h2.Logins[0].ID), "printer01", "", ""}, [4]string{fmt.Sprint(h2.Logins[1].ID), "reports", "", ""})
	v.Add("login_remove", fmt.Sprint(h2.Logins[0].ID))
	v.Add("login_remove", fmt.Sprint(h2.Logins[1].ID))
	e.post(t, edit, "/hosts/save", v)
	if h3, _ := e.store.GetHost(1); h3.RequiresLogin() {
		t.Fatalf("logins not removed: %+v", h3.Logins)
	}
}
