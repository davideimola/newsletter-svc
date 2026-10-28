package server

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// Each page is parsed together with the layout, into a set of its own, so every page can
// define the same `content` block.
var pages = func() map[string]*template.Template {
	funcs := template.FuncMap{
		"date": func(t any) string {
			switch v := t.(type) {
			case time.Time:
				return v.UTC().Format("2006-01-02 15:04 UTC")
			case *time.Time:
				if v != nil {
					return v.UTC().Format("2006-01-02 15:04 UTC")
				}
			}
			return "-"
		},
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
	}
	out := map[string]*template.Template{}
	for _, name := range []string{"message", "unsubscribe", "home", "subscribers", "subscriber", "issue"} {
		out[name] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/delivery.html", "templates/"+name+".html"))
	}
	return out
}()

func (s *Server) page(w http.ResponseWriter, status int, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The issue page frames its own preview, and nothing else may frame anything here.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	if err := pages[name].ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("template", "name", name, "err", err)
	}
}
