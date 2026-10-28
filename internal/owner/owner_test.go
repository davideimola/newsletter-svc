package owner_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davideimola/newsletter-svc/internal/owner"
)

type provider struct{}

func (provider) AuthCodeURL(state, _ string) string { return "https://idp.example.com/?state=" + state }
func (provider) Exchange(context.Context, string, string) (owner.Identity, error) {
	return owner.Identity{Email: "owner@example.com", EmailVerified: true}, nil
}

// A sign-in only ever lands on a path of this service.
func TestLoginLandsOnlyOnLocalPaths(t *testing.T) {
	g := &owner.Gate{Provider: provider{}, Email: "owner@example.com", Key: []byte(strings.Repeat("k", 32)), Now: time.Now,
		Denied: func(http.ResponseWriter, *http.Request, string) {}}
	for next, want := range map[string]string{
		"/admin/issues/2026-11": "/admin/issues/2026-11",
		"//evil.example":        "/admin",
		`/\evil.example`:        "/admin",
		"https://evil.example":  "/admin",
	} {
		login := httptest.NewRecorder()
		g.Login(login, httptest.NewRequest("GET", "/auth/login?next="+url.QueryEscape(next), nil))
		loc, _ := url.Parse(login.Header().Get("Location"))
		cb := httptest.NewRequest("GET", "/auth/callback?code=x&state="+url.QueryEscape(loc.Query().Get("state")), nil)
		for _, c := range login.Result().Cookies() {
			cb.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		g.Callback(rec, cb)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("next=%q landed on %q, want %q", next, got, want)
		}
	}
}
