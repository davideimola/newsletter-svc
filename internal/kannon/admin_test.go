package kannon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davideimola/newsletter-svc/internal/kannon"
)

func TestAdminReadsStatsAndTracking(t *testing.T) {
	var paths []string
	var statsReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("X-Kannon-Admin-Token") != "admin" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/kannon.stats.apiv2.StatsApiV2/GetAggregatedStats":
			json.NewDecoder(r.Body).Decode(&statsReq)
			w.Write([]byte(`{"stats":[{"type":"delivered","timestamp":"2026-10-28T21:00:00Z","count":"2"}]}`))
		case "/pkg.kannon.admin.apiv1.Api/GetDomain":
			w.Write([]byte(`{"domain":{"domain":"news.example.com","dkimPubKey":"x","tracking":{"opens":"TRACKING_MODE_OFF","links":"TRACKING_MODE_OFF"}}}`))
		}
	}))
	defer srv.Close()
	a := &kannon.Admin{URL: srv.URL, Token: "admin"}
	ctx := context.Background()

	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stats, err := a.AggregatedStats(ctx, "news.example.com", from, from.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Type != "delivered" || stats[0].Count != 2 || !stats[0].At.Equal(time.Date(2026, 10, 28, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("stats = %+v", stats)
	}
	if statsReq["domain"] != "news.example.com" || statsReq["fromDate"] != "2026-10-01T00:00:00Z" || statsReq["toDate"] != "2026-11-01T00:00:00Z" {
		t.Errorf("request = %v", statsReq)
	}

	tr, err := a.DomainTracking(ctx, "news.example.com")
	if err != nil || !tr.Off() {
		t.Fatalf("tracking = %+v, %v", tr, err)
	}

	bad := &kannon.Admin{URL: srv.URL, Token: "wrong"}
	if _, err := bad.DomainTracking(ctx, "news.example.com"); err == nil {
		t.Error("want an error for a refused token")
	}
}

func TestTrackingOffNeedsBoth(t *testing.T) {
	if (kannon.Tracking{Opens: "TRACKING_MODE_OFF", Links: "TRACKING_MODE_IDENTIFIED"}).Off() {
		t.Error("links tracked, yet Off() says off")
	}
}
