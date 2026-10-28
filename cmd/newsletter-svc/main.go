// Command newsletter-svc keeps davideimola.dev's newsletter subscribers and sends the issues
// through Kannon. See README.md.
//
//	newsletter-svc            serve (the default)
//	newsletter-svc migrate    apply the database migrations and exit
//	newsletter-svc erase EMAIL
//	                          delete one address and everything stored about it
//	newsletter-svc import-kit < export.csv
//	                          add Kit's active subscribers as confirmed
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davideimola/newsletter-svc/internal/config"
	"github.com/davideimola/newsletter-svc/internal/kannon"
	"github.com/davideimola/newsletter-svc/internal/newsletter"
	"github.com/davideimola/newsletter-svc/internal/owner"
	"github.com/davideimola/newsletter-svc/internal/server"
	"github.com/davideimola/newsletter-svc/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "erase":
		if len(os.Args) != 3 {
			err = errors.New("usage: newsletter-svc erase EMAIL")
			break
		}
		err = erase(ctx, os.Args[2])
	case "import-kit":
		err = importKit(ctx)
	default:
		err = fmt.Errorf("unknown command %q: serve, migrate, erase or import-kit", cmd)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context) (*store.Store, error) {
	dsn, err := config.DatabaseOnly()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, dsn)
}

func migrate(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

func erase(ctx context.Context, raw string) error {
	email, err := newsletter.NormalizeEmail(raw)
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	sub, err := st.SubscriberByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Println("No such address: nothing to delete.")
		return nil
	}
	if err != nil {
		return err
	}
	if err := st.Erase(ctx, sub.ID); err != nil {
		return err
	}
	fmt.Println("Deleted.")
	return nil
}

// importKit reads Kit's subscriber export on stdin, so the file never has to be copied into
// the container: `kubectl exec -i ... -- /newsletter-svc import-kit < export.csv`.
func importKit(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	svc := &newsletter.Service{Store: st, Now: time.Now}
	res, err := svc.ImportKit(ctx, os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf("Imported %d, already here %d, not active %d, invalid %d.\n",
		res.Imported, res.Existing, res.Skipped, res.Invalid)
	return nil
}

func serve(ctx context.Context) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	svc := &newsletter.Service{
		Store: st,
		Mailer: &kannon.Client{
			URL: cfg.KannonURL, Domain: cfg.KannonDomain, APIKey: cfg.KannonAPIKey,
			Sender: kannon.Sender{Email: cfg.SenderEmail, Alias: cfg.SenderName},
		},
		PublicURL:     cfg.PublicURL,
		SiteURL:       cfg.SiteURL,
		TestRecipient: cfg.TestRecipient,
		Now:           time.Now,
	}
	google, err := owner.NewGoogle(ctx, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.PublicURL+"/auth/callback")
	if err != nil {
		return err
	}
	public, _ := url.Parse(cfg.PublicURL)
	gate := &owner.Gate{
		Provider: google,
		Email:    cfg.OwnerEmail,
		Key:      []byte(cfg.SessionKey),
		Secure:   public.Scheme == "https",
		Now:      time.Now,
	}

	go cleanup(ctx, svc)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(svc, gate, cfg.APIKey),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "public", cfg.PublicURL)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cleanup deletes the never-confirmed addresses once an hour.
func cleanup(ctx context.Context, svc *newsletter.Service) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := svc.Cleanup(ctx)
		if err != nil {
			slog.Error("cleanup", "err", err)
		} else if n > 0 {
			slog.Info("deleted unconfirmed addresses", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
