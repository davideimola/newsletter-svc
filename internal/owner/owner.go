// Package owner is the gate in front of the admin pages: a sign-in with Google, an allowlist
// of exactly one email, and a signed session cookie. It is mcp-oauth-gateway's gate, unchanged
// but for the cookie names and where a sign-in lands.
package owner

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"crypto/rand"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Identity is what an identity provider vouches for.
type Identity struct {
	Email         string
	EmailVerified bool
}

// Provider is an OpenID Connect sign-in. Google in production, a fake in tests.
type Provider interface {
	AuthCodeURL(state, nonce string) string
	Exchange(ctx context.Context, code, nonce string) (Identity, error)
}

type google struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func NewGoogle(ctx context.Context, clientID, clientSecret, redirectURL string) (Provider, error) {
	p, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, err
	}
	return &google{
		oauth: &oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

func (g *google) AuthCodeURL(state, nonce string) string {
	return g.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.SetAuthURLParam("prompt", "select_account"))
}

func (g *google) Exchange(ctx context.Context, code, nonce string) (Identity, error) {
	tok, err := g.oauth.Exchange(ctx, code)
	if err != nil {
		return Identity{}, err
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("no id_token in the token response")
	}
	idt, err := g.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, err
	}
	if idt.Nonce != nonce {
		return Identity{}, errors.New("nonce mismatch")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, err
	}
	return Identity{Email: claims.Email, EmailVerified: claims.EmailVerified}, nil
}

// Gate signs the owner in and checks the session on every gated request.
type Gate struct {
	Provider Provider
	Email    string
	Key      []byte
	Secure   bool // set the Secure flag on cookies (off only for loopback development)
	// Denied renders the page shown to a signed-in account that is not the owner.
	Denied func(w http.ResponseWriter, r *http.Request, email string)
	Now    func() time.Time
}

const (
	sessionCookie = "nl_session"
	loginCookie   = "nl_login"
	sessionTTL    = 12 * time.Hour
	loginTTL      = 10 * time.Minute
)

// Owner reports whether the request carries a valid owner session.
func (g *Gate) Owner(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	payload, ok := g.verify(c.Value)
	if !ok {
		return false
	}
	email, exp, ok := strings.Cut(payload, "|")
	if !ok || !strings.EqualFold(email, g.Email) {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && g.Now().Unix() < unix
}

// CSRF returns a token bound to the current session, for forms behind the gate.
func (g *Gate) CSRF(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return g.mac("csrf|" + c.Value)
}

func (g *Gate) CheckCSRF(r *http.Request) bool {
	want := g.CSRF(r)
	return want != "" && hmac.Equal([]byte(want), []byte(r.PostFormValue("csrf")))
}

// Login starts the sign-in, remembering where to come back to. next must be a local path.
func (g *Gate) Login(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/admin"
	}
	state, nonce := random(), random()
	g.set(w, loginCookie, g.sign(state+"|"+nonce+"|"+next), loginTTL)
	http.Redirect(w, r, g.Provider.AuthCodeURL(state, nonce), http.StatusFound)
}

// Callback finishes the sign-in. Only the owner's verified email gets a session.
func (g *Gate) Callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginCookie)
	if err != nil {
		http.Error(w, "The sign-in expired. Start again.", http.StatusBadRequest)
		return
	}
	g.clear(w, loginCookie)
	payload, ok := g.verify(c.Value)
	parts := strings.SplitN(payload, "|", 3)
	if !ok || len(parts) != 3 || !hmac.Equal([]byte(parts[0]), []byte(r.URL.Query().Get("state"))) {
		http.Error(w, "The sign-in did not match. Start again.", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "The sign-in was cancelled: "+e, http.StatusForbidden)
		return
	}
	id, err := g.Provider.Exchange(r.Context(), r.URL.Query().Get("code"), parts[1])
	if err != nil {
		http.Error(w, "The sign-in failed.", http.StatusBadGateway)
		return
	}
	if !id.EmailVerified || !strings.EqualFold(id.Email, g.Email) {
		g.Denied(w, r, id.Email)
		return
	}
	g.set(w, sessionCookie, g.sign(fmt.Sprintf("%s|%d", g.Email, g.Now().Add(sessionTTL).Unix())), sessionTTL)
	http.Redirect(w, r, parts[2], http.StatusFound)
}

func (g *Gate) Logout(w http.ResponseWriter, r *http.Request) {
	g.clear(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

// SessionCookie mints a session cookie value, for tests.
func (g *Gate) SessionCookie() *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: g.sign(fmt.Sprintf("%s|%d", g.Email, g.Now().Add(sessionTTL).Unix()))}
}

func (g *Gate) set(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: g.Secure, SameSite: http.SameSiteLaxMode})
}

func (g *Gate) clear(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: g.Secure})
}

func (g *Gate) mac(s string) string {
	m := hmac.New(sha256.New, g.Key)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (g *Gate) sign(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + g.mac(payload)
}

func (g *Gate) verify(v string) (string, bool) {
	enc, sig, ok := strings.Cut(v, ".")
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	payload := string(raw)
	return payload, hmac.Equal([]byte(sig), []byte(g.mac(payload)))
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
