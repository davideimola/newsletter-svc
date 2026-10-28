package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/davideimola/newsletter-svc/internal/store"
)

const pageSize = 50

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request) {
	counts, err := s.svc.Store.Counts(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	issues, err := s.svc.Store.ListIssues(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	byName := map[string]int{}
	for st, n := range counts {
		byName[string(st)] = n
	}
	s.adminPage(w, r, http.StatusOK, "home", map[string]any{"Counts": byName, "Issues": issues})
}

func (s *Server) adminSubscribers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := store.Status(q.Get("status"))
	switch status {
	case "", store.Pending, store.Confirmed, store.Unsubscribed:
	default:
		status = ""
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	subs, total, err := s.svc.Store.ListSubscribers(r.Context(), store.SubscriberQuery{
		Status: status, Search: q.Get("q"), Limit: pageSize, Offset: (page - 1) * pageSize,
	})
	if err != nil {
		s.adminError(w, err)
		return
	}
	pageURL := func(p int) string {
		v := url.Values{}
		if status != "" {
			v.Set("status", string(status))
		}
		if q.Get("q") != "" {
			v.Set("q", q.Get("q"))
		}
		v.Set("page", strconv.Itoa(p))
		return "/admin/subscribers?" + v.Encode()
	}
	data := map[string]any{
		"Subscribers": subs, "Total": total, "Status": string(status), "Search": q.Get("q"),
		"Statuses": []store.Status{store.Confirmed, store.Pending, store.Unsubscribed},
	}
	if page > 1 {
		data["Prev"] = pageURL(page - 1)
	}
	if page*pageSize < total {
		data["Next"] = pageURL(page + 1)
	}
	s.adminPage(w, r, http.StatusOK, "subscribers", data)
}

func (s *Server) adminSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub, err := s.svc.Store.SubscriberByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.adminError(w, err)
		return
	}
	s.adminPage(w, r, http.StatusOK, "subscriber", map[string]any{"Subscriber": sub})
}

// adminErase deletes an address entirely: the deletion a subscriber can ask for by email.
func (s *Server) adminErase(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.PostFormValue("confirm") != "yes" {
		http.Redirect(w, r, "/admin/subscribers/"+r.PathValue("id"), http.StatusSeeOther)
		return
	}
	if err := s.svc.Store.Erase(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.adminError(w, err)
		return
	}
	slog.Info("subscriber erased", "id", id)
	http.Redirect(w, r, "/admin/subscribers?erased=1", http.StatusSeeOther)
}

func (s *Server) adminIssue(w http.ResponseWriter, r *http.Request) {
	issue, err := s.svc.Store.Issue(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.adminError(w, err)
		return
	}
	counts, err := s.svc.Store.Counts(r.Context())
	if err != nil {
		s.adminError(w, err)
		return
	}
	s.adminPage(w, r, http.StatusOK, "issue", map[string]any{
		"Issue": issue, "Confirmed": counts[store.Confirmed], "TestRecipient": s.svc.TestRecipient,
		"Notice": r.URL.Query().Get("notice"),
	})
}

// adminIssuePreview serves the issue's HTML for the iframe on its page, sandboxed: no script,
// no form, nothing loaded but images.
func (s *Server) adminIssuePreview(w http.ResponseWriter, r *http.Request) {
	issue, err := s.svc.Store.Issue(r.Context(), r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src https: data:; style-src 'unsafe-inline'; frame-ancestors 'self'")
	w.Write([]byte(issue.HTML))
}

func (s *Server) adminIssueTest(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	notice := "test-sent"
	if err := s.svc.TestSend(r.Context(), slug); err != nil {
		slog.Error("test send", "slug", slug, "err", err)
		notice = "test-failed"
	}
	http.Redirect(w, r, "/admin/issues/"+slug+"?notice="+notice, http.StatusSeeOther)
}

// adminIssueSend is the only way an issue reaches the list. The form asks for the slug typed
// out, so the button cannot be pressed by accident.
func (s *Server) adminIssueSend(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if r.PostFormValue("confirm") != slug {
		http.Redirect(w, r, "/admin/issues/"+slug+"?notice=confirm-mismatch", http.StatusSeeOther)
		return
	}
	notice := "sent"
	if _, err := s.svc.Send(r.Context(), slug); err != nil {
		slog.Error("send", "slug", slug, "err", err)
		switch {
		case errors.Is(err, store.ErrNotTested):
			notice = "not-tested"
		case errors.Is(err, store.ErrAlreadySent), errors.Is(err, store.ErrSending):
			notice = "already-sent"
		default:
			notice = "send-failed"
		}
	}
	http.Redirect(w, r, "/admin/issues/"+slug+"?notice="+notice, http.StatusSeeOther)
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	data["CSRF"] = s.gate.CSRF(r)
	data["Owner"] = s.gate.Email
	data["Admin"] = true
	s.page(w, status, name, data)
}

func (s *Server) adminError(w http.ResponseWriter, err error) {
	slog.Error("admin", "err", err)
	s.page(w, http.StatusInternalServerError, "message", map[string]any{
		"Title": "Something went wrong", "Body": err.Error(),
	})
}
