// Package store keeps subscribers and issues in Postgres.
//
// It holds no rules of its own beyond what keeps the rows consistent: what a subscription
// means, and when an issue may go out, live in package newsletter.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound    = errors.New("not found")
	ErrAlreadySent = errors.New("the issue has already been sent")
	ErrNotTested   = errors.New("the issue has not been test-sent since its last change")
	ErrSending     = errors.New("the issue is being sent")
)

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres. Query parameters pgx does not know, such as `search_path`, are
// passed on to the server as session settings, which is how the tests get a schema each.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies the embedded migrations that have not run yet, each in its own
// transaction, under an advisory lock so two processes never race on them.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const lock = 7_316_402_118 // any constant; it only has to be the same in every process
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, path := range names {
		name := strings.TrimPrefix(path, "migrations/")
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(path)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

type Status string

const (
	Pending      Status = "pending"
	Confirmed    Status = "confirmed"
	Unsubscribed Status = "unsubscribed"
)

type Subscriber struct {
	ID               int64
	Email            string
	Status           Status
	ConfirmTokenHash []byte
	ConfirmSentAt    *time.Time
	UnsubscribeToken string
	Source           string
	CreatedAt        time.Time
	ConfirmedAt      *time.Time
	UnsubscribedAt   *time.Time
}

const subscriberColumns = `id, email, status, confirm_token_hash, confirm_sent_at, unsubscribe_token,
	source, created_at, confirmed_at, unsubscribed_at`

func scanSubscriber(row pgx.Row) (*Subscriber, error) {
	var s Subscriber
	err := row.Scan(&s.ID, &s.Email, &s.Status, &s.ConfirmTokenHash, &s.ConfirmSentAt, &s.UnsubscribeToken,
		&s.Source, &s.CreatedAt, &s.ConfirmedAt, &s.UnsubscribedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

func (s *Store) SubscriberByEmail(ctx context.Context, email string) (*Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx, "SELECT "+subscriberColumns+" FROM subscribers WHERE email = $1", email))
}

func (s *Store) SubscriberByID(ctx context.Context, id int64) (*Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx, "SELECT "+subscriberColumns+" FROM subscribers WHERE id = $1", id))
}

func (s *Store) SubscriberByConfirmHash(ctx context.Context, hash []byte) (*Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx, "SELECT "+subscriberColumns+" FROM subscribers WHERE confirm_token_hash = $1", hash))
}

func (s *Store) SubscriberByUnsubscribeToken(ctx context.Context, token string) (*Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx, "SELECT "+subscriberColumns+" FROM subscribers WHERE unsubscribe_token = $1", token))
}

// CreatePending inserts a new address waiting for its confirmation click.
func (s *Store) CreatePending(ctx context.Context, email string, confirmHash []byte, unsubscribeToken string, now time.Time) (*Subscriber, error) {
	return scanSubscriber(s.pool.QueryRow(ctx, `INSERT INTO subscribers
		(email, status, confirm_token_hash, confirm_sent_at, unsubscribe_token, created_at)
		VALUES ($1, 'pending', $2, $3, $4, $3)
		RETURNING `+subscriberColumns, email, confirmHash, now, unsubscribeToken))
}

// ImportConfirmed adds an address that consented elsewhere, with the time it did. An address
// already here is left as it is, whatever its state: the list here is the newer truth.
func (s *Store) ImportConfirmed(ctx context.Context, email, unsubscribeToken, source string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO subscribers
		(email, status, unsubscribe_token, source, created_at, confirmed_at)
		VALUES ($1, 'confirmed', $2, $3, $4, $4)
		ON CONFLICT (email) DO NOTHING`, email, unsubscribeToken, source, at)
	return tag.RowsAffected() == 1, err
}

// Rearm puts an existing address back to pending with a fresh confirmation link: a pending
// address asking again, or an unsubscribed one subscribing again.
func (s *Store) Rearm(ctx context.Context, id int64, confirmHash []byte, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE subscribers SET status = 'pending', confirm_token_hash = $2,
		confirm_sent_at = $3, confirmed_at = NULL, unsubscribed_at = NULL
		WHERE id = $1 AND status <> 'confirmed'`, id, confirmHash, now)
	return err
}

func (s *Store) Confirm(ctx context.Context, id int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE subscribers SET status = 'confirmed', confirmed_at = $2
		WHERE id = $1 AND status = 'pending'`, id, now)
	return err
}

func (s *Store) Unsubscribe(ctx context.Context, id int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE subscribers SET status = 'unsubscribed', unsubscribed_at = $2
		WHERE id = $1 AND status <> 'unsubscribed'`, id, now)
	return err
}

// Erase deletes the row outright: the deletion a subscriber can ask for. Nothing about the
// address is left behind.
func (s *Store) Erase(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM subscribers WHERE id = $1", id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ClearConfirmSent forgets that a confirmation went out, after Kannon refused to send it, so
// the next request for the same address sends one rather than waiting out the cooldown.
func (s *Store) ClearConfirmSent(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, "UPDATE subscribers SET confirm_sent_at = NULL WHERE id = $1 AND status = 'pending'", id)
	return err
}

// DeleteStalePending removes the addresses that never confirmed, asked before the cutoff.
func (s *Store) DeleteStalePending(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM subscribers
		WHERE status = 'pending' AND COALESCE(confirm_sent_at, created_at) < $1`, before)
	return tag.RowsAffected(), err
}

func (s *Store) Counts(ctx context.Context) (map[Status]int, error) {
	rows, err := s.pool.Query(ctx, "SELECT status, count(*) FROM subscribers GROUP BY status")
	if err != nil {
		return nil, err
	}
	out := map[Status]int{Pending: 0, Confirmed: 0, Unsubscribed: 0}
	for rows.Next() {
		var st Status
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

type SubscriberQuery struct {
	Status Status // empty for every status
	Search string // a substring of the address
	Limit  int
	Offset int
}

func (s *Store) ListSubscribers(ctx context.Context, q SubscriberQuery) ([]Subscriber, int, error) {
	where := "WHERE ($1 = '' OR status = $1) AND ($2 = '' OR email LIKE '%' || $2 || '%')"
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM subscribers "+where, string(q.Status), strings.ToLower(q.Search)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+subscriberColumns+" FROM subscribers "+where+
		" ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4", string(q.Status), strings.ToLower(q.Search), q.Limit, q.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Subscriber
	for rows.Next() {
		sub, err := scanSubscriber(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *sub)
	}
	return out, total, rows.Err()
}

// Recipient is one confirmed address and the token its unsubscribe link carries.
type Recipient struct {
	Email            string
	UnsubscribeToken string
}

func (s *Store) ConfirmedRecipients(ctx context.Context) ([]Recipient, error) {
	rows, err := s.pool.Query(ctx, "SELECT email, unsubscribe_token FROM subscribers WHERE status = 'confirmed' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		var r Recipient
		if err := rows.Scan(&r.Email, &r.UnsubscribeToken); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type IssueStatus string

const (
	Draft   IssueStatus = "draft"
	Sending IssueStatus = "sending"
	Sent    IssueStatus = "sent"
)

type Issue struct {
	Slug            string
	Subject         string
	HTML            string
	Status          IssueStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
	TestSentAt      *time.Time
	SentAt          *time.Time
	Recipients      *int
	Accepted        *int
	Rejected        *int
	KannonMessageID *string
	LastError       *string
}

// Tested reports whether the last test send came after the last change.
func (i *Issue) Tested() bool {
	return i.TestSentAt != nil && !i.TestSentAt.Before(i.UpdatedAt)
}

const issueColumns = `slug, subject, html, status, created_at, updated_at, test_sent_at, sent_at,
	recipients, accepted, rejected, kannon_message_id, last_error`

func scanIssue(row pgx.Row) (*Issue, error) {
	var i Issue
	err := row.Scan(&i.Slug, &i.Subject, &i.HTML, &i.Status, &i.CreatedAt, &i.UpdatedAt, &i.TestSentAt, &i.SentAt,
		&i.Recipients, &i.Accepted, &i.Rejected, &i.KannonMessageID, &i.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

func (s *Store) Issue(ctx context.Context, slug string) (*Issue, error) {
	return scanIssue(s.pool.QueryRow(ctx, "SELECT "+issueColumns+" FROM issues WHERE slug = $1", slug))
}

func (s *Store) ListIssues(ctx context.Context) ([]Issue, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+issueColumns+" FROM issues ORDER BY slug DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// SaveDraft creates the issue or replaces its subject and HTML. A change after a test send
// makes the issue untested again. An issue that went out, or is going out, cannot change.
func (s *Store) SaveDraft(ctx context.Context, slug, subject, html string, now time.Time) (*Issue, error) {
	i, err := scanIssue(s.pool.QueryRow(ctx, `INSERT INTO issues (slug, subject, html, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'draft', $4, $4)
		ON CONFLICT (slug) DO UPDATE SET subject = $2, html = $3, updated_at = $4, last_error = NULL
		WHERE issues.status = 'draft'
		RETURNING `+issueColumns, slug, subject, html, now))
	if errors.Is(err, ErrNotFound) {
		return nil, s.sendingOrSent(ctx, slug)
	}
	return i, err
}

func (s *Store) MarkTestSent(ctx context.Context, slug string, now time.Time) error {
	_, err := s.pool.Exec(ctx, "UPDATE issues SET test_sent_at = $2 WHERE slug = $1", slug, now)
	return err
}

// BeginSend claims the issue for the real send. Only one caller can win it, and only while
// the issue is a draft whose latest version was test-sent.
func (s *Store) BeginSend(ctx context.Context, slug string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE issues SET status = 'sending', last_error = NULL
		WHERE slug = $1 AND status = 'draft' AND test_sent_at >= updated_at`, slug)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	i, err := s.Issue(ctx, slug)
	if err != nil {
		return err
	}
	if i.Status == Draft {
		return ErrNotTested
	}
	return s.sendingOrSent(ctx, slug)
}

func (s *Store) FinishSend(ctx context.Context, slug string, now time.Time, recipients, accepted, rejected int, messageID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE issues SET status = 'sent', sent_at = $2, recipients = $3,
		accepted = $4, rejected = $5, kannon_message_id = $6 WHERE slug = $1 AND status = 'sending'`,
		slug, now, recipients, accepted, rejected, messageID)
	return err
}

// FailSend returns the issue to draft after a send that Kannon refused outright, so nothing
// went out and it can be tried again.
func (s *Store) FailSend(ctx context.Context, slug, reason string) error {
	_, err := s.pool.Exec(ctx, "UPDATE issues SET status = 'draft', last_error = $2 WHERE slug = $1 AND status = 'sending'", slug, reason)
	return err
}

// NoteSendError records why a send's outcome is unknown, leaving the issue `sending`: the
// request may have reached Kannon, so trying again could mail everybody twice.
func (s *Store) NoteSendError(ctx context.Context, slug, reason string) error {
	_, err := s.pool.Exec(ctx, "UPDATE issues SET last_error = $2 WHERE slug = $1 AND status = 'sending'", slug, reason)
	return err
}

func (s *Store) sendingOrSent(ctx context.Context, slug string) error {
	i, err := s.Issue(ctx, slug)
	if err != nil {
		return err
	}
	if i.Status == Sending {
		return ErrSending
	}
	return ErrAlreadySent
}

// IsUniqueViolation reports whether err is Postgres refusing a duplicate key.
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
