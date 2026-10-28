package newsletter_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/davideimola/newsletter-svc/internal/store"
)

const kitExport = "\uFEFFfirst_name,email,created_at,status,tags,city,state,country\n" +
	",Reader@Example.org,2026-07-22 14:31:55 UTC,active,,Milan,Lombardy,IT\n" +
	",gone@example.org,2026-07-23 10:00:00 UTC,cancelled,,,,\n" +
	",not an address,2026-07-23 11:00:00 UTC,active,,,,\n" +
	",already@example.org,2026-07-24 09:00:00 UTC,active,,,,\n"

func TestImportKit(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := setup(t)
	svc.Subscribe(ctx, "already@example.org")
	sent := m.count()

	res, err := svc.ImportKit(ctx, strings.NewReader(kitExport))
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || res.Existing != 1 || res.Skipped != 1 || res.Invalid != 1 {
		t.Fatalf("result = %+v", res)
	}
	if m.count() != sent {
		t.Error("an import must not mail anybody")
	}

	sub, err := svc.Store.SubscriberByEmail(ctx, "reader@example.org")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 22, 14, 31, 55, 0, time.UTC)
	if sub.Status != store.Confirmed || sub.Source != "kit" || sub.ConfirmedAt == nil || !sub.ConfirmedAt.Equal(want) {
		t.Fatalf("imported: %+v", sub)
	}
	if sub.UnsubscribeToken == "" {
		t.Error("an imported subscriber needs an unsubscribe link like everybody else")
	}
	if got := status(t, svc, "already@example.org"); got != store.Pending {
		t.Errorf("an address already here was changed to %s", got)
	}

	// Running it twice changes nothing.
	again, _ := svc.ImportKit(ctx, strings.NewReader(kitExport))
	if again.Imported != 0 || again.Existing != 2 {
		t.Errorf("second run = %+v", again)
	}
}

func TestImportRefusesAnotherFormat(t *testing.T) {
	svc, _, _ := setup(t)
	if _, err := svc.ImportKit(context.Background(), strings.NewReader("address,date\na@example.org,x\n")); err == nil {
		t.Fatal("want an error for a file that is not Kit's export")
	}
}
