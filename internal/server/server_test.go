package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davideimola/newsletter-svc/internal/kannon"
	"github.com/davideimola/newsletter-svc/internal/newsletter"
	"github.com/davideimola/newsletter-svc/internal/owner"
	"github.com/davideimola/newsletter-svc/internal/server"
	"github.com/davideimola/newsletter-svc/internal/store"
	"github.com/davideimola/newsletter-svc/internal/store/storetest"
)

const (
	apiKey     = "an-api-key-that-is-at-least-32-characters"
	ownerEmail = "owner@example.com"
)

type mailer struct {
	mu   sync.Mutex
	sent []kannon.Message
}

func (m *mailer) Send(_ context.Context, msg kannon.Message) (*kannon.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return &kannon.Result{MessageID: "msg_1", Accepted: len(msg.Recipients)}, nil
}

func (m *mailer) last() kannon.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

func (m *mailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// provider stands in for Google: it signs in whoever the test says.
type provider struct{ id owner.Identity }

func (p *provider) AuthCodeURL(state, nonce string) string {
	return "https://idp.example.com/auth?state=" + url.QueryEscape(state)
}

func (p *provider) Exchange(context.Context, string, string) (owner.Identity, error) {
	return p.id, nil
}

type env struct {
	srv  *httptest.Server
	svc  *newsletter.Service
	mail *mailer
	gate *owner.Gate
	now  time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{mail: &mailer{}, now: time.Now()}
	e.svc = &newsletter.Service{
		Store: storetest.New(t), Mailer: e.mail,
		PublicURL: "https://news.example.com", SiteURL: "https://example.com",
		TestRecipient: ownerEmail, Now: func() time.Time { return e.now },
	}
	e.gate = &owner.Gate{
		Provider: &provider{id: owner.Identity{Email: ownerEmail, EmailVerified: true}},
		Email:    ownerEmail, Key: []byte("a-session-key-that-is-at-least-32-bytes"), Now: time.Now,
	}
	e.srv = httptest.NewServer(server.New(e.svc, e.gate, apiKey))
	t.Cleanup(e.srv.Close)
	return e
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) api(t *testing.T, method, path, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func read(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

var tokenIn = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

func TestAPINeedsTheKey(t *testing.T) {
	e := setup(t)
	for _, key := range []string{"", "wrong"} {
		if resp := e.api(t, "POST", "/api/subscribers", key, `{"email":"a@example.org"}`); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status %d", key, resp.StatusCode)
		}
	}
	if e.mail.count() != 0 {
		t.Error("mail sent without the key")
	}
}

func TestSubscribeConfirmUnsubscribe(t *testing.T) {
	e := setup(t)
	resp := e.api(t, "POST", "/api/subscribers", apiKey, `{"email":"Reader@Example.org"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("subscribe: %d %s", resp.StatusCode, read(t, resp))
	}
	again := e.api(t, "POST", "/api/subscribers", apiKey, `{"email":"reader@example.org"}`)
	if again.StatusCode != resp.StatusCode || read(t, again) != `{"status":"check_your_inbox"}`+"\n" {
		t.Error("a second request must get the same answer")
	}
	if bad := e.api(t, "POST", "/api/subscribers", apiKey, `{"email":"nope"}`); bad.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid address: %d", bad.StatusCode)
	}

	confirm := tokenIn.FindStringSubmatch(e.mail.last().HTML)[1]
	r, err := noRedirect().Get(e.srv.URL + "/confirm?token=" + confirm)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "https://example.com/newsletter/confirmed" {
		t.Fatalf("confirm: %d -> %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := http.Get(e.srv.URL + "/confirm?token=bogus"); r.StatusCode != http.StatusNotFound {
		t.Errorf("bogus confirm: %d", r.StatusCode)
	}

	sub, _ := e.svc.Store.SubscriberByEmail(context.Background(), "reader@example.org")
	page, _ := http.Get(e.srv.URL + "/unsubscribe?token=" + sub.UnsubscribeToken)
	if body := read(t, page); !strings.Contains(body, "reader@example.org") {
		t.Fatalf("the unsubscribe page does not name the address:\n%s", body)
	}
	if s, _ := e.svc.Store.SubscriberByEmail(context.Background(), "reader@example.org"); s.Status != store.Confirmed {
		t.Fatal("opening the page alone must not unsubscribe: link scanners open every link")
	}

	// RFC 8058: a POST with List-Unsubscribe=One-Click unsubscribes with no further step.
	one, err := http.PostForm(e.srv.URL+"/unsubscribe?token="+sub.UnsubscribeToken, url.Values{"List-Unsubscribe": {"One-Click"}})
	if err != nil || one.StatusCode != http.StatusOK {
		t.Fatalf("one-click: %v %d", err, one.StatusCode)
	}
	if s, _ := e.svc.Store.SubscriberByEmail(context.Background(), "reader@example.org"); s.Status != store.Unsubscribed {
		t.Fatalf("status after one-click = %s", s.Status)
	}
	if r, _ := http.PostForm(e.srv.URL+"/unsubscribe?token=bogus", url.Values{"List-Unsubscribe": {"One-Click"}}); r.StatusCode != http.StatusOK {
		t.Errorf("one-click with an unknown token: %d, want 200", r.StatusCode)
	}
}

func TestAPICannotSendToTheList(t *testing.T) {
	e := setup(t)
	resp := e.api(t, "PUT", "/api/issues/2026-11", apiKey, `{"subject":"November","html":"<a href=\"{{ unsubscribe_url }}\">u</a>"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save: %d %s", resp.StatusCode, read(t, resp))
	}
	if r := e.api(t, "PUT", "/api/issues/2026-11", apiKey, `{"subject":"November","html":"<p>no link</p>"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an issue without the unsubscribe link: %d", r.StatusCode)
	}
	if r := e.api(t, "POST", "/api/issues/2026-11/test", apiKey, ``); r.StatusCode != http.StatusOK {
		t.Fatalf("test: %d %s", r.StatusCode, read(t, r))
	}
	if got := e.mail.last().Recipients; len(got) != 1 || got[0].Email != ownerEmail {
		t.Fatalf("test went to %+v", got)
	}
	if r := e.api(t, "POST", "/api/issues/2026-11/send", apiKey, ``); r.StatusCode != http.StatusNotFound && r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("the API has a send route: %d", r.StatusCode)
	}
}

func TestAdminIsTheOwnersOnly(t *testing.T) {
	e := setup(t)
	r, _ := noRedirect().Get(e.srv.URL + "/admin/subscribers")
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), "/auth/login") {
		t.Fatalf("anonymous admin: %d -> %s", r.StatusCode, r.Header.Get("Location"))
	}

	// Someone else's Google account gets no session.
	e.gate.Provider = &provider{id: owner.Identity{Email: "someone@example.com", EmailVerified: true}}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == "idp.example.com" {
			// Skip Google: come straight back to the callback with the state it was given.
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, _ := c.Get(e.srv.URL + "/admin")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	cb, _ := c.Get(e.srv.URL + "/auth/callback?code=x&state=" + url.QueryEscape(loc.Query().Get("state")))
	if cb.StatusCode != http.StatusForbidden {
		t.Fatalf("another account: %d", cb.StatusCode)
	}
}

func TestAdminSendsAfterTypedConfirmation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.svc.Subscribe(ctx, "a@example.org")
	e.svc.Confirm(ctx, tokenIn.FindStringSubmatch(e.mail.last().HTML)[1])
	e.svc.SaveIssue(ctx, "2026-11", "November", `<a href="{{ unsubscribe_url }}">u</a>`)
	e.now = e.now.Add(time.Minute)
	e.svc.TestSend(ctx, "2026-11")
	before := e.mail.count()

	session := e.gate.SessionCookie()
	post := func(path string, form url.Values) *http.Response {
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		resp, err := noRedirect().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	reqForCSRF, _ := http.NewRequest("GET", "/", nil)
	reqForCSRF.AddCookie(session)
	csrf := e.gate.CSRF(reqForCSRF)

	if r := post("/admin/issues/2026-11/send", url.Values{"confirm": {"2026-11"}}); r.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", r.StatusCode)
	}
	r := post("/admin/issues/2026-11/send", url.Values{"csrf": {csrf}, "confirm": {"2026-1"}})
	if !strings.Contains(r.Header.Get("Location"), "confirm-mismatch") || e.mail.count() != before {
		t.Fatalf("a mistyped slug sent the issue: %s", r.Header.Get("Location"))
	}
	r = post("/admin/issues/2026-11/send", url.Values{"csrf": {csrf}, "confirm": {"2026-11"}})
	if !strings.Contains(r.Header.Get("Location"), "notice=sent") {
		t.Fatalf("send: %d -> %s", r.StatusCode, r.Header.Get("Location"))
	}
	if got := e.mail.last().Recipients; len(got) != 1 || got[0].Email != "a@example.org" {
		t.Fatalf("sent to %+v", got)
	}

	// The pages render for the owner.
	for _, path := range []string{"/admin", "/admin/subscribers", "/admin/subscribers?status=confirmed&q=exa", "/admin/issues/2026-11"} {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		req.AddCookie(session)
		resp, err := noRedirect().Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %v %d", path, err, resp.StatusCode)
		}
	}
}

func TestAdminErase(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.svc.Subscribe(ctx, "a@example.org")
	sub, _ := e.svc.Store.SubscriberByEmail(ctx, "a@example.org")

	session := e.gate.SessionCookie()
	reqForCSRF, _ := http.NewRequest("GET", "/", nil)
	reqForCSRF.AddCookie(session)
	form := url.Values{"csrf": {e.gate.CSRF(reqForCSRF)}, "confirm": {"yes"}}
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/subscribers/"+strconv.FormatInt(sub.ID, 10)+"/erase", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	if _, err := noRedirect().Do(req); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Store.SubscriberByEmail(ctx, "a@example.org"); err != store.ErrNotFound {
		t.Fatalf("the address survived: %v", err)
	}
}
