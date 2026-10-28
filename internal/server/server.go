// Package server is the HTTP face of the service: the API the website calls, the links in the
// emails, and the owner's admin pages.
//
//	/api/...          bearer API_KEY      the website and its scripts
//	/confirm          public              the link in the confirmation email
//	/unsubscribe      public              the link in every issue, and the one-click POST
//	/admin/...        Google, one owner   the list and the issues
//
// The API can save an issue and test-send it, but it cannot send it to the list: the real send
// is a button on the admin pages, so it always takes a person (the website's ADR-0002).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/davideimola/newsletter-svc/internal/newsletter"
	"github.com/davideimola/newsletter-svc/internal/owner"
	"github.com/davideimola/newsletter-svc/internal/store"
)

type Server struct {
	svc          *newsletter.Service
	gate         *owner.Gate
	apiKey       string
	mux          *http.ServeMux
	delivery     Delivery
	kannonDomain string
}

func New(svc *newsletter.Service, gate *owner.Gate, apiKey string) *Server {
	s := &Server{svc: svc, gate: gate, apiKey: apiKey, mux: http.NewServeMux()}
	gate.Denied = func(w http.ResponseWriter, r *http.Request, email string) {
		s.page(w, http.StatusForbidden, "message", map[string]any{
			"Title": "Not this account",
			"Body":  "You signed in as " + email + ", which is not the owner of this newsletter.",
		})
	}

	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /{$}", s.home)

	s.mux.Handle("POST /api/subscribers", s.api(s.apiSubscribe))
	s.mux.Handle("PUT /api/issues/{slug}", s.api(s.apiSaveIssue))
	s.mux.Handle("POST /api/issues/{slug}/test", s.api(s.apiTestIssue))

	s.mux.HandleFunc("GET /confirm", s.confirm)
	s.mux.HandleFunc("GET /unsubscribe", s.unsubscribePage)
	s.mux.HandleFunc("POST /unsubscribe", s.unsubscribe)

	s.mux.HandleFunc("GET /auth/login", gate.Login)
	s.mux.HandleFunc("GET /auth/callback", gate.Callback)
	s.mux.HandleFunc("POST /auth/logout", gate.Logout)

	s.mux.Handle("GET /admin", s.owner(s.adminHome))
	s.mux.Handle("GET /admin/subscribers", s.owner(s.adminSubscribers))
	s.mux.Handle("GET /admin/subscribers/{id}", s.owner(s.adminSubscriber))
	s.mux.Handle("POST /admin/subscribers/{id}/erase", s.owner(s.adminErase))
	s.mux.Handle("GET /admin/issues/{slug}", s.owner(s.adminIssue))
	s.mux.Handle("GET /admin/issues/{slug}/preview", s.owner(s.adminIssuePreview))
	s.mux.Handle("POST /admin/issues/{slug}/test", s.owner(s.adminIssueTest))
	s.mux.Handle("POST /admin/issues/{slug}/send", s.owner(s.adminIssueSend))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

// home is where a sign-out lands. It does not bounce straight into the sign-in: after a form
// post, CSP's form-action would stop the redirect chain at Google's door.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if s.gate.Owner(r) {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.page(w, http.StatusOK, "message", map[string]any{
		"Title": "Davide Imola's newsletter",
		"Body":  "This is the server behind it. The newsletter itself lives on the website.",
		"Link":  s.svc.SiteURL + "/newsletter", "LinkText": "Go to the newsletter",
		"SignIn": true,
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Store.Ping(r.Context()); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

// api checks the bearer token the website presents.
func (s *Server) api(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	})
}

// owner sends anyone without the owner's session to the sign-in, and checks the CSRF token on
// every form post.
func (s *Server) owner(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.gate.Owner(r) {
			if r.Method != http.MethodGet {
				http.Error(w, "Sign in again.", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/auth/login?next="+r.URL.RequestURI(), http.StatusFound)
			return
		}
		if r.Method == http.MethodPost && !s.gate.CheckCSRF(r) {
			http.Error(w, "The form expired. Go back and try again.", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) apiSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	err := s.svc.Subscribe(r.Context(), body.Email)
	switch {
	case errors.Is(err, newsletter.ErrInvalidEmail):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_email"})
	case err != nil:
		slog.Error("subscribe", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "unavailable"})
	default:
		// The same answer for a new, a waiting and an already confirmed address.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "check_your_inbox"})
	}
}

type issueJSON struct {
	Slug       string `json:"slug"`
	Subject    string `json:"subject"`
	Status     string `json:"status"`
	Tested     bool   `json:"tested"`
	AdminURL   string `json:"adminUrl"`
	Recipients *int   `json:"recipients,omitempty"`
}

func (s *Server) issueJSON(i *store.Issue) issueJSON {
	return issueJSON{Slug: i.Slug, Subject: i.Subject, Status: string(i.Status), Tested: i.Tested(),
		AdminURL: s.svc.PublicURL + "/admin/issues/" + i.Slug, Recipients: i.Recipients}
}

func (s *Server) apiSaveIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Subject string `json:"subject"`
		HTML    string `json:"html"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	issue, err := s.svc.SaveIssue(r.Context(), r.PathValue("slug"), body.Subject, body.HTML)
	if err != nil {
		s.issueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.issueJSON(issue))
}

func (s *Server) apiTestIssue(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := s.svc.TestSend(r.Context(), slug); err != nil {
		s.issueError(w, err)
		return
	}
	issue, err := s.svc.Store.Issue(r.Context(), slug)
	if err != nil {
		s.issueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.issueJSON(issue))
}

func (s *Server) issueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, newsletter.ErrInvalidIssue):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_issue"})
	case errors.Is(err, newsletter.ErrNoUnsubscribeLink):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_unsubscribe_url"})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	case errors.Is(err, store.ErrAlreadySent), errors.Is(err, store.ErrSending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already_sent"})
	default:
		slog.Error("issue", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "unavailable"})
	}
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	err := s.svc.Confirm(r.Context(), r.URL.Query().Get("token"))
	switch {
	case err == nil:
		http.Redirect(w, r, s.svc.SiteURL+"/newsletter/confirmed", http.StatusSeeOther)
	case errors.Is(err, newsletter.ErrExpiredLink):
		s.page(w, http.StatusGone, "message", map[string]any{
			"Title": "This link has expired",
			"Body":  "Confirmation links work for seven days. Subscribe again and a new one will reach you.",
			"Link":  s.svc.SiteURL + "/newsletter", "LinkText": "Subscribe again",
		})
	case errors.Is(err, newsletter.ErrInvalidLink):
		s.page(w, http.StatusNotFound, "message", map[string]any{
			"Title": "This link does not work",
			"Body":  "It may have been copied only in part, or replaced by a newer one.",
			"Link":  s.svc.SiteURL + "/newsletter", "LinkText": "Subscribe again",
		})
	default:
		slog.Error("confirm", "err", err)
		s.page(w, http.StatusInternalServerError, "message", map[string]any{
			"Title": "Something went wrong", "Body": "It is on my side. Try the link again in a moment.",
		})
	}
}

func (s *Server) unsubscribePage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "test" {
		s.page(w, http.StatusOK, "message", map[string]any{
			"Title": "This is a test send",
			"Body":  "In the real issue this link unsubscribes the reader who received it.",
		})
		return
	}
	sub, err := s.svc.Unsubscriber(r.Context(), token)
	if err != nil {
		s.invalidUnsubscribe(w, err)
		return
	}
	if sub.Status != store.Confirmed {
		s.page(w, http.StatusOK, "message", map[string]any{
			"Title": "You are not subscribed",
			"Body":  sub.Email + " receives no newsletter from me.",
		})
		return
	}
	s.page(w, http.StatusOK, "unsubscribe", map[string]any{"Email": sub.Email, "Token": token})
}

// unsubscribe serves both the button on the page and the one-click POST a mail client makes
// from the List-Unsubscribe header (RFC 8058), which must work with no further step.
func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		token = r.PostFormValue("token")
	}
	oneClick := r.PostFormValue("List-Unsubscribe") == "One-Click"
	err := s.svc.Unsubscribe(r.Context(), token)
	if oneClick {
		if err != nil && !errors.Is(err, newsletter.ErrInvalidLink) {
			slog.Error("one-click unsubscribe", "err", err)
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if token == "test" {
		s.unsubscribePage(w, r)
		return
	}
	if err != nil {
		s.invalidUnsubscribe(w, err)
		return
	}
	s.page(w, http.StatusOK, "message", map[string]any{
		"Title": "You are unsubscribed",
		"Body":  "No more issues will reach you. Thank you for reading.",
		"Link":  s.svc.SiteURL, "LinkText": "Back to davideimola.dev",
	})
}

func (s *Server) invalidUnsubscribe(w http.ResponseWriter, err error) {
	if errors.Is(err, newsletter.ErrInvalidLink) {
		s.page(w, http.StatusNotFound, "message", map[string]any{
			"Title": "This link does not work",
			"Body":  "It may have been copied only in part. Reply to any issue and I will remove you by hand.",
		})
		return
	}
	slog.Error("unsubscribe", "err", err)
	s.page(w, http.StatusInternalServerError, "message", map[string]any{
		"Title": "Something went wrong", "Body": "It is on my side. Try the link again in a moment.",
	})
}
