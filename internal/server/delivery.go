package server

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/davideimola/newsletter-svc/internal/kannon"
)

// Delivery is what the admin pages read from Kannon: aggregated counters and the Domain's
// Tracking Policy. Nothing in it names a recipient.
type Delivery interface {
	AggregatedStats(ctx context.Context, domain string, from, to time.Time) ([]kannon.Count, error)
	DomainTracking(ctx context.Context, domain string) (kannon.Tracking, error)
}

// WithDelivery turns on the delivery panel. Without it the admin pages simply leave it out.
func (s *Server) WithDelivery(d Delivery, domain string) *Server {
	s.delivery, s.kannonDomain = d, domain
	return s
}

// The counters shown, in order. Kannon also counts `opened` and `clicked`; with tracking off
// they stay at zero, and the panel says so loudly if they ever do not.
var deliveryTypes = []string{"accepted", "delivered", "bounced", "failed", "error", "rejected"}

type deliveryRow struct {
	Day    string
	Counts []int64 // in deliveryTypes order
}

type deliveryPanel struct {
	Types    []string
	Totals   []int64
	Days     []deliveryRow
	Tracked  int64 // opened + clicked: anything but zero is a fault
	Tracking *kannon.Tracking
	Error    string
}

// deliveryFor reads the counters between from and to, by day, newest first. It never fails
// the page: a Kannon that does not answer becomes a line on the panel.
func (s *Server) deliveryFor(ctx context.Context, from, to time.Time, withDays bool) *deliveryPanel {
	if s.delivery == nil {
		return nil
	}
	p := &deliveryPanel{Types: deliveryTypes, Totals: make([]int64, len(deliveryTypes))}
	tr, err := s.delivery.DomainTracking(ctx, s.kannonDomain)
	if err == nil {
		p.Tracking = &tr
	}
	counts, serr := s.delivery.AggregatedStats(ctx, s.kannonDomain, from, to)
	if err == nil {
		err = serr
	}
	if err != nil {
		slog.Error("delivery panel", "err", err)
		p.Error = "Kannon did not answer: " + err.Error()
	}

	index := map[string]int{}
	for i, t := range deliveryTypes {
		index[t] = i
	}
	days := map[string][]int64{}
	for _, c := range counts {
		if c.Type == "opened" || c.Type == "clicked" {
			p.Tracked += c.Count
			continue
		}
		i, ok := index[c.Type]
		if !ok {
			continue
		}
		p.Totals[i] += c.Count
		day := c.At.UTC().Format("2006-01-02")
		if days[day] == nil {
			days[day] = make([]int64, len(deliveryTypes))
		}
		days[day][i] += c.Count
	}
	if withDays {
		for day, row := range days {
			p.Days = append(p.Days, deliveryRow{Day: day, Counts: row})
		}
		sort.Slice(p.Days, func(i, j int) bool { return p.Days[i].Day > p.Days[j].Day })
	}
	return p
}
