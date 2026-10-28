// Package config reads the service's settings from the environment. README.md lists them.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Listen      string
	DatabaseURL string
	// PublicURL is this service's own origin, e.g. https://news.davideimola.dev.
	PublicURL string
	// SiteURL is the website's origin, e.g. https://davideimola.dev.
	SiteURL string
	// APIKey is what the website presents, as a bearer token, to /api.
	APIKey string

	KannonURL    string
	KannonDomain string
	KannonAPIKey string
	SenderEmail  string
	SenderName   string
	// TestRecipient receives every test send.
	TestRecipient string

	OwnerEmail         string
	GoogleClientID     string
	GoogleClientSecret string
	// SessionKey signs the owner's session cookie. At least 32 bytes.
	SessionKey string
}

func FromEnv() (*Config, error) {
	var missing []string
	get := func(name string) string {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	opt := func(name, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	c := &Config{
		Listen:             opt("LISTEN_ADDR", ":8080"),
		DatabaseURL:        get("DATABASE_URL"),
		PublicURL:          strings.TrimRight(get("PUBLIC_URL"), "/"),
		SiteURL:            strings.TrimRight(get("SITE_URL"), "/"),
		APIKey:             get("API_KEY"),
		KannonURL:          get("KANNON_URL"),
		KannonDomain:       get("KANNON_DOMAIN"),
		KannonAPIKey:       get("KANNON_API_KEY"),
		SenderEmail:        get("SENDER_EMAIL"),
		SenderName:         get("SENDER_NAME"),
		TestRecipient:      get("TEST_RECIPIENT"),
		OwnerEmail:         get("OWNER_EMAIL"),
		GoogleClientID:     get("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: get("GOOGLE_CLIENT_SECRET"),
		SessionKey:         get("SESSION_KEY"),
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}
	return c, c.validate()
}

// DatabaseOnly reads just what the `migrate` and `erase` commands need.
func DatabaseOnly() (string, error) {
	v := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if v == "" {
		return "", errors.New("missing environment variable: DATABASE_URL")
	}
	return v, nil
}

func (c *Config) validate() error {
	var errs []error
	for name, v := range map[string]string{"PUBLIC_URL": c.PublicURL, "SITE_URL": c.SiteURL, "KANNON_URL": c.KannonURL} {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" {
			errs = append(errs, fmt.Errorf("%s must be an origin like https://example.com, got %q", name, v))
		}
	}
	if len(c.APIKey) < 32 {
		errs = append(errs, errors.New("API_KEY must be at least 32 characters"))
	}
	if len(c.SessionKey) < 32 {
		errs = append(errs, errors.New("SESSION_KEY must be at least 32 characters"))
	}
	return errors.Join(errs...)
}
