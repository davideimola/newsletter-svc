// Package newsletter is what a subscription means and when an issue may go out.
//
// The rules, in one place:
//
//   - An address is subscribed only after it clicks the link in the confirmation email
//     (double opt-in). The click's time is the proof of consent.
//   - Asking to subscribe never reveals whether an address is already on the list: every
//     request gets the same answer.
//   - A confirmation link works for ConfirmWindow. An address that never confirms is deleted
//     after PendingRetention.
//   - An unsubscribed address is never mailed again unless it subscribes, and confirms, again.
//   - An issue goes out once. It must be test-sent after its last change first.
package newsletter

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/davideimola/newsletter-svc/internal/kannon"
	"github.com/davideimola/newsletter-svc/internal/store"
)

const (
	ConfirmWindow    = 7 * 24 * time.Hour
	PendingRetention = 30 * 24 * time.Hour
	// ResendCooldown stops the form from being used to flood a mailbox with confirmations.
	ResendCooldown = 10 * time.Minute
)

var (
	ErrInvalidEmail      = errors.New("not a valid email address")
	ErrInvalidLink       = errors.New("the link is not valid")
	ErrExpiredLink       = errors.New("the link has expired")
	ErrInvalidIssue      = errors.New("the issue is not valid")
	ErrNoUnsubscribeLink = errors.New("the issue HTML has no {{ unsubscribe_url }}")
)

type Mailer interface {
	Send(ctx context.Context, m kannon.Message) (*kannon.Result, error)
}

type Service struct {
	Store  *store.Store
	Mailer Mailer
	// PublicURL is this service's origin, the base of the confirmation and unsubscribe links.
	PublicURL string
	// SiteURL is the website's origin: where a confirmed address lands, and where the
	// subscribe form lives.
	SiteURL string
	// TestRecipient receives the test sends.
	TestRecipient string
	Now           func() time.Time
}

// NormalizeEmail returns the address as it is stored, or ErrInvalidEmail.
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) > 254 {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return "", ErrInvalidEmail
	}
	_, domain, _ := strings.Cut(email, "@")
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// Subscribe starts the double opt-in for an address. It returns nil whether the address is
// new, waiting, already confirmed or unsubscribed, so the caller cannot tell them apart.
func (s *Service) Subscribe(ctx context.Context, raw string) error {
	email, err := NormalizeEmail(raw)
	if err != nil {
		return err
	}
	now := s.Now()
	token, hash := newConfirmToken()

	sub, err := s.Store.SubscriberByEmail(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		sub, err = s.Store.CreatePending(ctx, email, hash, secret(), now)
		if store.IsUniqueViolation(err) {
			return nil // the same address, submitted twice at once: the other request mails it
		}
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case sub.Status == store.Confirmed:
		return nil
	case sub.Status == store.Pending && sub.ConfirmSentAt != nil && now.Sub(*sub.ConfirmSentAt) < ResendCooldown:
		return nil
	default:
		if err := s.Store.Rearm(ctx, sub.ID, hash, now); err != nil {
			return err
		}
	}

	if err := s.sendConfirmation(ctx, email, token); err != nil {
		s.Store.ClearConfirmSent(ctx, sub.ID)
		return err
	}
	return nil
}

// Confirm turns the token of a confirmation link into a subscription. A link clicked twice
// confirms once and succeeds both times.
func (s *Service) Confirm(ctx context.Context, token string) error {
	sub, err := s.Store.SubscriberByConfirmHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return ErrInvalidLink
	}
	if err != nil {
		return err
	}
	switch sub.Status {
	case store.Confirmed:
		return nil
	case store.Pending:
		if sub.ConfirmSentAt != nil && s.Now().Sub(*sub.ConfirmSentAt) > ConfirmWindow {
			return ErrExpiredLink
		}
		return s.Store.Confirm(ctx, sub.ID, s.Now())
	}
	return ErrInvalidLink
}

// Unsubscriber returns the subscriber an unsubscribe link belongs to.
func (s *Service) Unsubscriber(ctx context.Context, token string) (*store.Subscriber, error) {
	if token == "" {
		return nil, ErrInvalidLink
	}
	sub, err := s.Store.SubscriberByUnsubscribeToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidLink
	}
	return sub, err
}

func (s *Service) Unsubscribe(ctx context.Context, token string) error {
	sub, err := s.Unsubscriber(ctx, token)
	if err != nil {
		return err
	}
	return s.Store.Unsubscribe(ctx, sub.ID, s.Now())
}

// Cleanup deletes the addresses that asked to subscribe and never confirmed.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	return s.Store.DeleteStalePending(ctx, s.Now().Add(-PendingRetention))
}

var (
	slugPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	unsubscribePattern = regexp.MustCompile(`\{\{ *unsubscribe_url *\}\}`)
)

// SaveIssue stores an issue as the site rendered it, ready for its test send.
func (s *Service) SaveIssue(ctx context.Context, slug, subject, html string) (*store.Issue, error) {
	subject = strings.TrimSpace(subject)
	if !slugPattern.MatchString(slug) || subject == "" || strings.TrimSpace(html) == "" {
		return nil, ErrInvalidIssue
	}
	if !unsubscribePattern.MatchString(html) {
		return nil, ErrNoUnsubscribeLink
	}
	return s.Store.SaveDraft(ctx, slug, subject, html, s.Now())
}

// TestSend mails the issue to TestRecipient only. Its unsubscribe link is a dummy that
// unsubscribes nobody.
func (s *Service) TestSend(ctx context.Context, slug string) error {
	issue, err := s.Store.Issue(ctx, slug)
	if err != nil {
		return err
	}
	if issue.Status != store.Draft {
		return store.ErrAlreadySent
	}
	_, err = s.Mailer.Send(ctx, kannon.Message{
		Subject: "[Test] " + issue.Subject,
		HTML:    issue.HTML,
		Recipients: []kannon.Recipient{{Email: s.TestRecipient, Fields: map[string]string{
			"token": "test", "unsubscribe_url": s.unsubscribeURL("test"),
		}}},
		UnsubscribeURL: s.PublicURL + "/unsubscribe?token={{ token }}",
	})
	if err != nil {
		return err
	}
	return s.Store.MarkTestSent(ctx, slug, s.Now())
}

// Send mails the issue to every confirmed subscriber, once.
func (s *Service) Send(ctx context.Context, slug string) (*store.Issue, error) {
	if err := s.Store.BeginSend(ctx, slug); err != nil {
		return nil, err
	}
	issue, err := s.Store.Issue(ctx, slug)
	if err != nil {
		return nil, err
	}
	recipients, err := s.Store.ConfirmedRecipients(ctx)
	if err != nil {
		s.Store.FailSend(ctx, slug, err.Error())
		return nil, err
	}
	if len(recipients) == 0 {
		return s.finish(ctx, slug, 0, &kannon.Result{})
	}
	to := make([]kannon.Recipient, len(recipients))
	for i, r := range recipients {
		to[i] = kannon.Recipient{Email: r.Email, Fields: map[string]string{
			"token": r.UnsubscribeToken, "unsubscribe_url": s.unsubscribeURL(r.UnsubscribeToken),
		}}
	}
	res, err := s.Mailer.Send(ctx, kannon.Message{
		Subject:        issue.Subject,
		HTML:           issue.HTML,
		Recipients:     to,
		UnsubscribeURL: s.PublicURL + "/unsubscribe?token={{ token }}",
	})
	var refused *kannon.RefusedError
	switch {
	case errors.As(err, &refused):
		s.Store.FailSend(ctx, slug, err.Error())
		return nil, err
	case err != nil:
		s.Store.NoteSendError(ctx, slug, "outcome unknown, check Kannon before doing anything: "+err.Error())
		return nil, err
	}
	return s.finish(ctx, slug, len(recipients), res)
}

func (s *Service) finish(ctx context.Context, slug string, n int, res *kannon.Result) (*store.Issue, error) {
	if err := s.Store.FinishSend(ctx, slug, s.Now(), n, res.Accepted, len(res.Rejected), res.MessageID); err != nil {
		return nil, err
	}
	return s.Store.Issue(ctx, slug)
}

func (s *Service) unsubscribeURL(token string) string {
	return s.PublicURL + "/unsubscribe?token=" + token
}

func (s *Service) sendConfirmation(ctx context.Context, email, token string) error {
	html, err := renderConfirmation(s.PublicURL+"/confirm?token="+token, s.SiteURL)
	if err != nil {
		return err
	}
	_, err = s.Mailer.Send(ctx, kannon.Message{
		Subject:    confirmationSubject,
		HTML:       html,
		Recipients: []kannon.Recipient{{Email: email}},
	})
	if err != nil {
		return fmt.Errorf("send the confirmation: %w", err)
	}
	return nil
}

// secret is 32 random bytes, URL-safe. It is the whole of every token: nothing in it is
// derived from the address.
func secret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newConfirmToken() (string, []byte) {
	t := secret()
	return t, hashToken(t)
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}
