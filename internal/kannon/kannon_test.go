package kannon_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davideimola/newsletter-svc/internal/kannon"
)

func TestSendSpeaksKannonsJSON(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"messageId":"msg_x@news.example.com","acceptedCount":1,"rejectedRecipients":[{"email":"b@example.org","reason":"invalid_email"}]}`))
	}))
	defer srv.Close()

	c := &kannon.Client{URL: srv.URL + "/", Domain: "news.example.com", APIKey: "k_secret",
		Sender: kannon.Sender{Email: "hello@example.com", Alias: "Hello"}}
	res, err := c.Send(context.Background(), kannon.Message{
		Subject:        "Hi",
		HTML:           "<p>x</p>",
		Recipients:     []kannon.Recipient{{Email: "a@example.org", Fields: map[string]string{"token": "t"}}},
		UnsubscribeURL: "https://news.example.com/unsubscribe?token={{ token }}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/pkg.kannon.mailer.apiv1.Mailer/SendHTML" {
		t.Errorf("path = %s", path)
	}
	if auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("news.example.com:k_secret")) {
		t.Errorf("auth = %s", auth)
	}
	tracking, _ := got["tracking"].(map[string]any)
	if tracking["opens"] != "TRACKING_MODE_OFF" || tracking["links"] != "TRACKING_MODE_OFF" {
		t.Errorf("tracking = %v, want both off", got["tracking"])
	}
	unsub, _ := got["oneClickUnsubscribe"].(map[string]any)
	if unsub["urlTemplate"] != "https://news.example.com/unsubscribe?token={{ token }}" {
		t.Errorf("oneClickUnsubscribe = %v", got["oneClickUnsubscribe"])
	}
	sender, _ := got["sender"].(map[string]any)
	if sender["email"] != "hello@example.com" || sender["alias"] != "Hello" {
		t.Errorf("sender = %v", got["sender"])
	}
	if res.MessageID != "msg_x@news.example.com" || res.Accepted != 1 || len(res.Rejected) != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestNoUnsubscribeHeaderWhenNotAsked(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"messageId":"m","acceptedCount":1}`))
	}))
	defer srv.Close()
	c := &kannon.Client{URL: srv.URL, Domain: "d", APIKey: "k"}
	if _, err := c.Send(context.Background(), kannon.Message{Recipients: []kannon.Recipient{{Email: "a@example.org"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["oneClickUnsubscribe"]; ok {
		t.Error("oneClickUnsubscribe sent for a message that is not the newsletter")
	}
}

func TestRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"unauthenticated"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &kannon.Client{URL: srv.URL, Domain: "d", APIKey: "k"}
	_, err := c.Send(context.Background(), kannon.Message{})
	var refused *kannon.RefusedError
	if !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a RefusedError with 401", err)
	}
}
