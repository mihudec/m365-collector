package collector

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Poll commits a blob only after every event has been written successfully.
// stdout has no downstream acknowledgment: this is not an exactly-once protocol.
func Poll(ctx context.Context, c Config, a *API, s *State, out io.Writer, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	end := now.Add(-c.Lag).Truncate(time.Second)
	for _, kind := range c.ContentTypes {
		key := scope(c.TenantID, kind)
		cursor, e := s.cursor(key)
		if e != nil {
			return e
		}
		start := end.Add(-c.Lookback)
		if !cursor.IsZero() {
			start = cursor.Add(-c.Overlap)
		} else {
			// Freeze the initial lower bound even if the first request fails.
			if e = s.advance(key, start); e != nil {
				return e
			}
		}
		if start.Before(now.Add(-7*24*time.Hour + time.Minute)) {
			return fmt.Errorf("checkpoint for %s is outside API retention; use a new state path to explicitly accept the gap", kind)
		}
		for start.Before(end) {
			until := start.Add(24 * time.Hour)
			if until.After(end) {
				until = end
			}
			e = a.List(ctx, kind, start, until, func(blob Blob) error {
				done, e := s.done(key, blob.ID)
				if e != nil || done {
					return e
				}
				events, e := a.Events(ctx, blob)
				if e != nil {
					return e
				}
				for _, event := range events {
					if e = ctx.Err(); e != nil {
						return e
					}
					if e = writeEvent(out, event, c.Wrap); e != nil {
						return fmt.Errorf("write event: %w", e)
					}
				}
				return s.mark(key, blob.ID, now)
			})
			if e != nil {
				return fmt.Errorf("collect %s: %w", kind, e)
			}
			if e = s.advance(key, until); e != nil {
				return e
			}
			start = until
		}
	}
	return s.prune(now)
}
