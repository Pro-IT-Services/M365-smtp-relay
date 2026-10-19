package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"

	"graphrelay/internal/store"
)

// Optional SMTP AUTH per host rule. A host with logins must match by IP *and* log in with one of
// them; a host without logins keeps working by IP alone and AUTH isn't offered to it. A login may
// carry its own mailbox, so one machine can send as different mailboxes depending on the login.
// go-smtp only offers AUTH on TLS connections (STARTTLS or port 465), because
// Server.AllowInsecureAuth stays false.

const (
	authMaxFailures = 5
	authLockout     = 15 * time.Minute
)

var (
	errAuthFailed  = smtpErr(535, smtp.EnhancedCode{5, 7, 8}, "Authentication credentials invalid")
	errAuthLocked  = smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Too many failed logins, try again later")
	errAuthNotHere = smtpErr(503, smtp.EnhancedCode{5, 5, 1}, "Authentication is not enabled for this host")
)

// failLimiter counts failed logins per client IP and locks an IP out after too many.
type failLimiter struct {
	mu    sync.Mutex
	fails map[netip.Addr][]time.Time
}

func (l *failLimiter) locked(ip netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip, now)) >= authMaxFailures
}

func (l *failLimiter) fail(ip netip.Addr, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[netip.Addr][]time.Time{}
	}
	l.fails[ip] = append(l.recent(ip, now), now)
}

func (l *failLimiter) reset(ip netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// recent drops failures older than the lockout window. Caller holds mu.
func (l *failLimiter) recent(ip netip.Addr, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.fails[ip] {
		if now.Sub(t) < authLockout {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return keep
}

// HashSMTPPassword returns the bcrypt hash stored for an SMTP login's password.
func HashSMTPPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// AuthMechanisms is called for EHLO. AUTH is offered only when a rule for the client's address
// has logins.
func (s *session) AuthMechanisms() []string {
	hosts, err := s.relay.Matcher.MatchAll(context.Background(), s.ip)
	if err != nil {
		return nil
	}
	for i := range hosts {
		if hosts[i].RequiresLogin() {
			return []string{sasl.Plain, sasl.Login}
		}
	}
	return nil
}

// dummyHash is compared against when no login has the given username, so a wrong username takes
// as long as a wrong password and doesn't reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("no such login"), bcrypt.DefaultCost)

func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, user, pass string) error {
			if identity != "" && identity != user {
				return s.checkLogin("", "") // authorization identity must match; count as a failure
			}
			return s.checkLogin(user, pass)
		}), nil
	case sasl.Login:
		return &loginServer{check: s.checkLogin}, nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

// checkLogin verifies credentials against the logins of every host rule that matches the client
// IP, most specific rule first. The rule and login that accept them decide the mailbox.
func (s *session) checkLogin(user, pass string) error {
	now := time.Now()
	if s.relay.authFails.locked(s.ip, now) {
		s.relay.Log.Warn("smtp login refused: locked out", "ip", s.ip, "user", user)
		return errAuthLocked
	}
	hosts, err := s.relay.Matcher.MatchAll(context.Background(), s.ip)
	if err != nil {
		return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary server error, try again later")
	}
	var withLogins []*store.Host
	for i := range hosts {
		if hosts[i].RequiresLogin() {
			withLogins = append(withLogins, &hosts[i])
		}
	}
	if len(withLogins) == 0 {
		return errAuthNotHere
	}
	s.note(func(i *connInfo) { i.authUser = user })

	tried := false
	for _, h := range withLogins {
		for j := range h.Logins {
			l := &h.Logins[j]
			if subtle.ConstantTimeCompare([]byte(user), []byte(l.Username)) != 1 {
				continue
			}
			tried = true
			if bcrypt.CompareHashAndPassword([]byte(l.PassHash), []byte(pass)) != nil {
				continue
			}
			s.relay.authFails.reset(s.ip)
			s.authHostID, s.authLoginID = h.ID, l.ID
			s.note(func(i *connInfo) { i.authOK, i.hostID, i.hostName = true, h.ID, h.Name })
			s.relay.Log.Info("smtp login", "ip", s.ip, "host", h.Name, "user", user, "mailbox", h.SenderFor(l))
			return nil
		}
	}
	if !tried {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(pass))
	}

	best := withLogins[0]
	s.note(func(i *connInfo) { i.authFails++ })
	s.relay.authFails.fail(s.ip, now)
	s.record(&store.LogEntry{Status: "rejected", HostID: best.ID, HostName: best.Name, Sender: best.Sender,
		Error: fmt.Sprintf("SMTP login failed for user %q", truncate(user, 64))})
	s.relay.Log.Warn("smtp login failed", "ip", s.ip, "host", best.Name, "user", truncate(user, 64))
	return errAuthFailed
}

// loginServer implements the non-standard but widely used AUTH LOGIN mechanism
// (many printers and scanners support nothing else). go-sasl only ships a client.
type loginServer struct {
	check    func(user, pass string) error
	step     int
	username string
}

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.step {
	case 0:
		l.step = 1
		if len(response) == 0 { // no initial response: ask for the username
			return []byte("Username:"), false, nil
		}
		fallthrough
	case 1:
		l.username = string(response)
		l.step = 2
		return []byte("Password:"), false, nil
	case 2:
		l.step = 3
		return nil, true, l.check(l.username, string(response))
	}
	return nil, false, errors.New("unexpected client response")
}
