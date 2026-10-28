package newsletter_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davideimola/newsletter-svc/internal/kannon"
	"github.com/davideimola/newsletter-svc/internal/newsletter"
	"github.com/davideimola/newsletter-svc/internal/store"
	"github.com/davideimola/newsletter-svc/internal/store/storetest"
)

// mailer stands in for Kannon: it records every message and fails when told to.
type mailer struct {
	mu   sync.Mutex
	sent []kannon.Message
	err  error
}

func (m *mailer) Send(_ context.Context, msg kannon.Message) (*kannon.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.sent = append(m.sent, msg)
	return &kannon.Result{MessageID: "msg_1@news.example.com", Accepted: len(msg.Recipients)}, nil
}

func (m *mailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mailer) last() kannon.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T) (*newsletter.Service, *mailer, *clock) {
	t.Helper()
	m := &mailer{}
	c := &clock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	return &newsletter.Service{
		Store:         storetest.New(t),
		Mailer:        m,
		PublicURL:     "https://news.example.com",
		SiteURL:       "https://example.com",
		TestRecipient: "owner@example.com",
		Now:           c.now,
	}, m, c
}

var confirmLink = regexp.MustCompile(`https://news\.example\.com/confirm\?token=([A-Za-z0-9_-]+)`)

func confirmToken(t *testing.T, m kannon.Message) string {
	t.Helper()
	match := confirmLink.FindStringSubmatch(m.HTML)
	if match == nil {
		t.Fatalf("no confirmation link in:\n%s", m.HTML)
	}
	return match[1]
}

func status(t *testing.T, svc *newsletter.Service, email string) store.Status {
	t.Helper()
	sub, err := svc.Store.SubscriberByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return sub.Status
}

func TestDoubleOptIn(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := setup(t)

	if err := svc.Subscribe(ctx, "  Reader@Example.org "); err != nil {
		t.Fatal(err)
	}
	if got := status(t, svc, "reader@example.org"); got != store.Pending {
		t.Fatalf("status = %s, want pending", got)
	}
	msg := m.last()
	if len(msg.Recipients) != 1 || msg.Recipients[0].Email != "reader@example.org" {
		t.Fatalf("confirmation went to %+v", msg.Recipients)
	}
	if msg.UnsubscribeURL != "" {
		t.Error("the confirmation email must not advertise an unsubscribe header")
	}
	if strings.Contains(strings.ToLower(msg.HTML), "<img") {
		t.Error("the confirmation email must carry no image")
	}

	token := confirmToken(t, msg)
	if err := svc.Confirm(ctx, token); err != nil {
		t.Fatal(err)
	}
	sub, _ := svc.Store.SubscriberByEmail(ctx, "reader@example.org")
	if sub.Status != store.Confirmed || sub.ConfirmedAt == nil {
		t.Fatalf("after the click: %+v", sub)
	}
	// A second click on the same link is still a success.
	if err := svc.Confirm(ctx, token); err != nil {
		t.Fatalf("second click: %v", err)
	}
}

func TestSubscribeRevealsNothing(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	svc.Subscribe(ctx, "reader@example.org")
	svc.Confirm(ctx, confirmToken(t, m.last()))
	before := m.count()

	c.advance(time.Hour)
	if err := svc.Subscribe(ctx, "reader@example.org"); err != nil {
		t.Fatalf("a confirmed address subscribing again: %v", err)
	}
	if m.count() != before {
		t.Error("a confirmed address must not be mailed again")
	}
}

func TestResendCooldown(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	svc.Subscribe(ctx, "reader@example.org")
	first := confirmToken(t, m.last())

	c.advance(time.Minute)
	svc.Subscribe(ctx, "reader@example.org")
	if m.count() != 1 {
		t.Fatalf("mailed %d times inside the cooldown, want 1", m.count())
	}

	c.advance(newsletter.ResendCooldown)
	svc.Subscribe(ctx, "reader@example.org")
	if m.count() != 2 {
		t.Fatalf("mailed %d times after the cooldown, want 2", m.count())
	}
	if err := svc.Confirm(ctx, first); !errors.Is(err, newsletter.ErrInvalidLink) {
		t.Errorf("the replaced link: err = %v, want ErrInvalidLink", err)
	}
	if err := svc.Confirm(ctx, confirmToken(t, m.last())); err != nil {
		t.Errorf("the new link: %v", err)
	}
}

func TestConfirmationExpires(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	svc.Subscribe(ctx, "reader@example.org")
	c.advance(newsletter.ConfirmWindow + time.Minute)
	if err := svc.Confirm(ctx, confirmToken(t, m.last())); !errors.Is(err, newsletter.ErrExpiredLink) {
		t.Fatalf("err = %v, want ErrExpiredLink", err)
	}
	if got := status(t, svc, "reader@example.org"); got != store.Pending {
		t.Fatalf("status = %s, want pending", got)
	}
}

func TestFailedConfirmationCanBeRetriedAtOnce(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := setup(t)
	m.err = &kannon.RefusedError{Status: 503}
	if err := svc.Subscribe(ctx, "reader@example.org"); err == nil {
		t.Fatal("want the send error back")
	}
	m.err = nil
	if err := svc.Subscribe(ctx, "reader@example.org"); err != nil {
		t.Fatal(err)
	}
	if m.count() != 1 {
		t.Fatalf("the retry inside the cooldown sent %d, want 1", m.count())
	}
}

func TestInvalidEmails(t *testing.T) {
	svc, m, _ := setup(t)
	for _, e := range []string{"", "nope", "a@b", "Name <a@example.org>", "a@example.org.", "a @example.org"} {
		if err := svc.Subscribe(context.Background(), e); !errors.Is(err, newsletter.ErrInvalidEmail) {
			t.Errorf("%q: err = %v, want ErrInvalidEmail", e, err)
		}
	}
	if m.count() != 0 {
		t.Error("nothing should have been sent")
	}
}

func TestUnsubscribeAndComeBack(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	svc.Subscribe(ctx, "reader@example.org")
	svc.Confirm(ctx, confirmToken(t, m.last()))
	sub, _ := svc.Store.SubscriberByEmail(ctx, "reader@example.org")

	if err := svc.Unsubscribe(ctx, sub.UnsubscribeToken); err != nil {
		t.Fatal(err)
	}
	if got := status(t, svc, "reader@example.org"); got != store.Unsubscribed {
		t.Fatalf("status = %s", got)
	}
	if r, _ := svc.Store.ConfirmedRecipients(ctx); len(r) != 0 {
		t.Fatalf("an unsubscribed address is still a recipient: %+v", r)
	}
	if err := svc.Unsubscribe(ctx, "not-a-token"); !errors.Is(err, newsletter.ErrInvalidLink) {
		t.Errorf("unknown token: err = %v", err)
	}

	// Subscribing again takes a fresh confirmation.
	c.advance(time.Hour)
	svc.Subscribe(ctx, "reader@example.org")
	if got := status(t, svc, "reader@example.org"); got != store.Pending {
		t.Fatalf("status after subscribing again = %s, want pending", got)
	}
	svc.Confirm(ctx, confirmToken(t, m.last()))
	if got := status(t, svc, "reader@example.org"); got != store.Confirmed {
		t.Fatalf("status = %s, want confirmed", got)
	}
}

func TestCleanupDeletesOnlyStalePending(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	svc.Subscribe(ctx, "stale@example.org")
	svc.Subscribe(ctx, "kept@example.org")
	svc.Confirm(ctx, confirmToken(t, m.last()))

	c.advance(newsletter.PendingRetention + time.Hour)
	svc.Subscribe(ctx, "fresh@example.org")
	n, err := svc.Cleanup(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup = %d, %v; want 1", n, err)
	}
	if _, err := svc.Store.SubscriberByEmail(ctx, "stale@example.org"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the stale pending address survived")
	}
	for _, e := range []string{"kept@example.org", "fresh@example.org"} {
		if _, err := svc.Store.SubscriberByEmail(ctx, e); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
}

const issueHTML = `<p>Hello</p><a href="{{ unsubscribe_url }}">Unsubscribe</a>`

func confirmed(t *testing.T, svc *newsletter.Service, m *mailer, emails ...string) {
	t.Helper()
	for _, e := range emails {
		if err := svc.Subscribe(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if err := svc.Confirm(context.Background(), confirmToken(t, m.last())); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIssueNeedsUnsubscribeLink(t *testing.T) {
	svc, _, _ := setup(t)
	_, err := svc.SaveIssue(context.Background(), "2026-11", "November", "<p>no link</p>")
	if !errors.Is(err, newsletter.ErrNoUnsubscribeLink) {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.SaveIssue(context.Background(), "../x", "November", issueHTML); !errors.Is(err, newsletter.ErrInvalidIssue) {
		t.Fatalf("bad slug: err = %v", err)
	}
}

func TestIssueGoesOutOnceAfterATest(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	confirmed(t, svc, m, "a@example.org", "b@example.org")
	svc.Subscribe(ctx, "waiting@example.org")

	if _, err := svc.SaveIssue(ctx, "2026-11", "November", issueHTML); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, "2026-11"); !errors.Is(err, store.ErrNotTested) {
		t.Fatalf("send before a test: err = %v", err)
	}

	c.advance(time.Minute)
	if err := svc.TestSend(ctx, "2026-11"); err != nil {
		t.Fatal(err)
	}
	test := m.last()
	if len(test.Recipients) != 1 || test.Recipients[0].Email != "owner@example.com" || !strings.HasPrefix(test.Subject, "[Test] ") {
		t.Fatalf("test send: %+v", test)
	}

	// A change after the test locks the send again.
	c.advance(time.Minute)
	svc.SaveIssue(ctx, "2026-11", "November, fixed", issueHTML)
	if _, err := svc.Send(ctx, "2026-11"); !errors.Is(err, store.ErrNotTested) {
		t.Fatalf("send after a change: err = %v", err)
	}
	c.advance(time.Minute)
	svc.TestSend(ctx, "2026-11")

	c.advance(time.Minute)
	issue, err := svc.Send(ctx, "2026-11")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Status != store.Sent || *issue.Recipients != 2 || *issue.Accepted != 2 {
		t.Fatalf("issue after send: %+v", issue)
	}
	msg := m.last()
	if msg.Subject != "November, fixed" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if msg.UnsubscribeURL != "https://news.example.com/unsubscribe?token={{ token }}" {
		t.Errorf("one-click template = %q", msg.UnsubscribeURL)
	}
	var got []string
	for _, r := range msg.Recipients {
		got = append(got, r.Email)
		if r.Fields["token"] == "" || r.Fields["unsubscribe_url"] != "https://news.example.com/unsubscribe?token="+r.Fields["token"] {
			t.Errorf("%s fields: %+v", r.Email, r.Fields)
		}
	}
	if strings.Join(got, ",") != "a@example.org,b@example.org" {
		t.Errorf("recipients = %v, want only the confirmed ones", got)
	}

	if _, err := svc.Send(ctx, "2026-11"); !errors.Is(err, store.ErrAlreadySent) {
		t.Errorf("second send: err = %v", err)
	}
	if _, err := svc.SaveIssue(ctx, "2026-11", "x", issueHTML); !errors.Is(err, store.ErrAlreadySent) {
		t.Errorf("editing a sent issue: err = %v", err)
	}
	if err := svc.TestSend(ctx, "2026-11"); !errors.Is(err, store.ErrAlreadySent) {
		t.Errorf("testing a sent issue: err = %v", err)
	}
}

func TestRefusedSendCanBeRetried(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	confirmed(t, svc, m, "a@example.org")
	svc.SaveIssue(ctx, "2026-11", "November", issueHTML)
	c.advance(time.Minute)
	svc.TestSend(ctx, "2026-11")

	m.err = &kannon.RefusedError{Status: 400, Body: "bad"}
	if _, err := svc.Send(ctx, "2026-11"); err == nil {
		t.Fatal("want the refusal back")
	}
	issue, _ := svc.Store.Issue(ctx, "2026-11")
	if issue.Status != store.Draft || issue.LastError == nil {
		t.Fatalf("after a refusal: %+v", issue)
	}
	m.err = nil
	if _, err := svc.Send(ctx, "2026-11"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestUnknownOutcomeIsNotRetried(t *testing.T) {
	ctx := context.Background()
	svc, m, c := setup(t)
	confirmed(t, svc, m, "a@example.org")
	svc.SaveIssue(ctx, "2026-11", "November", issueHTML)
	c.advance(time.Minute)
	svc.TestSend(ctx, "2026-11")

	m.err = errors.New("connection reset")
	svc.Send(ctx, "2026-11")
	issue, _ := svc.Store.Issue(ctx, "2026-11")
	if issue.Status != store.Sending {
		t.Fatalf("status = %s, want sending", issue.Status)
	}
	m.err = nil
	if _, err := svc.Send(ctx, "2026-11"); !errors.Is(err, store.ErrSending) {
		t.Fatalf("a send with an unknown outcome must not go again: err = %v", err)
	}
}
