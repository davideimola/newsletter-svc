// Package kannon sends mail through Kannon's Mailer API (github.com/kannon-email/kannon).
//
// Kannon speaks Connect, so a call is a JSON POST to /<service>/<method>. Only SendHTML is
// used. Every call states a Tracking Policy of `off` for opens and links, although the
// Domain's own policy is already `off`: the Domain is the ceiling, and saying it here too
// means a Domain changed by mistake still tracks nothing that this service sends.
package kannon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	URL    string // e.g. http://kannon.kannon.svc:50051
	Domain string // the Kannon Domain the key belongs to
	APIKey string
	Sender Sender
	HTTP   *http.Client
}

type Sender struct {
	Email string `json:"email"`
	Alias string `json:"alias"`
}

type Recipient struct {
	Email  string            `json:"email"`
	Fields map[string]string `json:"fields,omitempty"`
}

type Message struct {
	Subject    string
	HTML       string
	Recipients []Recipient
	// UnsubscribeURL is the one-click endpoint (RFC 8058) as a template, e.g.
	// https://news.example.com/unsubscribe?token={{ token }}. Empty for mail that is not
	// the newsletter itself, like the confirmation email.
	UnsubscribeURL string
}

type Result struct {
	MessageID string
	Accepted  int
	Rejected  []Rejected
}

type Rejected struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

type off struct {
	Opens string `json:"opens"`
	Links string `json:"links"`
}

type sendHTMLReq struct {
	Sender      Sender      `json:"sender"`
	Subject     string      `json:"subject"`
	HTML        string      `json:"html"`
	Recipients  []Recipient `json:"recipients"`
	Tracking    off         `json:"tracking"`
	Unsubscribe *struct {
		URLTemplate string `json:"urlTemplate"`
	} `json:"oneClickUnsubscribe,omitempty"`
}

type sendRes struct {
	MessageID          string     `json:"messageId"`
	AcceptedCount      int        `json:"acceptedCount"`
	RejectedRecipients []Rejected `json:"rejectedRecipients"`
}

func (c *Client) Send(ctx context.Context, m Message) (*Result, error) {
	req := sendHTMLReq{
		Sender:     c.Sender,
		Subject:    m.Subject,
		HTML:       m.HTML,
		Recipients: m.Recipients,
		Tracking:   off{Opens: "TRACKING_MODE_OFF", Links: "TRACKING_MODE_OFF"},
	}
	if m.UnsubscribeURL != "" {
		req.Unsubscribe = &struct {
			URLTemplate string `json:"urlTemplate"`
		}{m.UnsubscribeURL}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.URL, "/")+"/pkg.kannon.mailer.apiv1.Mailer/SendHTML", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Domain+":"+c.APIKey)))
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &RefusedError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var out sendRes
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("kannon: decode response: %w", err)
	}
	return &Result{MessageID: out.MessageID, Accepted: out.AcceptedCount, Rejected: out.RejectedRecipients}, nil
}

// RefusedError is Kannon answering, and answering no. Nothing was queued.
type RefusedError struct {
	Status int
	Body   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("kannon refused the send (%d): %s", e.Status, e.Body)
}
