package newsletter

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ImportResult counts what an import did. It names no address, so it can be printed.
type ImportResult struct {
	Imported int // new confirmed subscribers
	Existing int // already here, left as they were
	Skipped  int // not active in the export
	Invalid  int // not an address this service accepts
}

// ImportKit reads a subscriber export from Kit (Subscribers, Export) and adds every `active`
// row as confirmed, with Kit's `created_at` as the time of consent: Kit's export has no
// confirmation date, and a Kit subscriber is active only once confirmed. Only the address
// and that time are kept; the location Kit guessed from the IP and the other columns are
// dropped.
func (s *Service) ImportKit(ctx context.Context, r io.Reader) (ImportResult, error) {
	var res ImportResult
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return res, fmt.Errorf("read the header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimPrefix(strings.TrimSpace(h), "\uFEFF")] = i
	}
	for _, need := range []string{"email", "created_at", "status"} {
		if _, ok := col[need]; !ok {
			return res, fmt.Errorf("the export has no %q column: is it Kit's subscriber export?", need)
		}
	}
	for {
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		if row[col["status"]] != "active" {
			res.Skipped++
			continue
		}
		email, err := NormalizeEmail(row[col["email"]])
		if err != nil {
			res.Invalid++
			continue
		}
		at, err := time.Parse("2006-01-02 15:04:05 MST", row[col["created_at"]])
		if err != nil {
			return res, fmt.Errorf("row %d: created_at %q: %w", res.Imported+res.Existing+res.Skipped+res.Invalid+2, row[col["created_at"]], err)
		}
		added, err := s.Store.ImportConfirmed(ctx, email, secret(), "kit", at.UTC())
		if err != nil {
			return res, err
		}
		if added {
			res.Imported++
		} else {
			res.Existing++
		}
	}
}
