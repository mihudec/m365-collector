package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

func NewGraphAPI(c Config, secret string) *API {
	a := NewAPI(c, secret)
	a.base = "https://graph.microsoft.com/" + c.GraphVersion + "/auditLogs/"
	a.oauthScope = "https://graph.microsoft.com/.default"
	return a
}

// GraphSignIns follows the opaque nextLink without modifying its query.
func (a *API) GraphSignIns(ctx context.Context, version, kind string, start, end time.Time, consume func(json.RawMessage, string) error) error {
	filter := "createdDateTime ge " + start.UTC().Format(time.RFC3339) + " and createdDateTime lt " + end.UTC().Format(time.RFC3339)
	if version == "beta" {
		filter = "(" + filter + ") and signInEventTypes/any(t: t eq '" + kind + "')"
	}
	next := a.base + "signIns?" + url.Values{"$filter": {filter}, "$top": {"1000"}}.Encode()
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return errors.New("Graph pagination cycle detected")
		}
		seen[next] = true
		u, e := url.Parse(next)
		base, _ := url.Parse(a.base)
		if e != nil || u.Path != base.Path+"signIns" {
			return errors.New("Graph returned an untrusted pagination URL")
		}
		b, _, e := a.request(ctx, "GET", next, nil, true)
		if e != nil {
			return e
		}
		var page struct {
			Value json.RawMessage `json:"value"`
			Next  string          `json:"@odata.nextLink"`
		}
		if json.Unmarshal(b, &page) != nil || len(page.Value) == 0 || bytes.Equal(bytes.TrimSpace(page.Value), []byte("null")) {
			return errors.New("invalid Graph sign-in page")
		}
		var events []json.RawMessage
		if json.Unmarshal(page.Value, &events) != nil {
			return errors.New("invalid Graph sign-in array")
		}
		for _, event := range events {
			var metadata struct {
				ID      string `json:"id"`
				Created string `json:"createdDateTime"`
			}
			if json.Unmarshal(event, &metadata) != nil || strings.TrimSpace(metadata.ID) == "" {
				return errors.New("Graph sign-in missing id")
			}
			created, e := time.Parse(time.RFC3339Nano, metadata.Created)
			if e != nil {
				return errors.New("Graph sign-in has invalid createdDateTime")
			}
			if created.Before(start) || !created.Before(end) {
				return errors.New("Graph sign-in is outside requested window")
			}
			if e = consume(event, metadata.ID); e != nil {
				return e
			}
		}
		next = page.Next
	}
	return nil
}

// PollGraph uses independent per-version/category cursors and event-ID hashes.
// The full time window is checkpointed only after every page succeeds.
func PollGraph(ctx context.Context, c Config, a *API, s *State, out io.Writer, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	end := now.Add(-c.Lag).Truncate(time.Second)
	for _, kind := range c.SignInTypes {
		key := scope(c.TenantID, "graph:"+c.GraphVersion+":"+kind)
		cursor, e := s.cursor(key)
		if e != nil {
			return e
		}
		start := end.Add(-c.Lookback)
		if !cursor.IsZero() {
			start = cursor.Add(-c.Overlap)
		}
		if start.Before(now.Add(-30*24*time.Hour + time.Minute)) {
			return fmt.Errorf("Graph checkpoint for %s is outside 30-day retention; use a new state path to explicitly accept the gap", kind)
		}
		if cursor.IsZero() {
			if e = s.advance(key, start); e != nil {
				return e
			}
		}
		for start.Before(end) {
			until := start.Add(time.Hour)
			if until.After(end) {
				until = end
			}
			e = a.GraphSignIns(ctx, c.GraphVersion, kind, start, until, func(event json.RawMessage, id string) error {
				if e := ctx.Err(); e != nil {
					return e
				}
				done, e := s.done(key, id)
				if e != nil || done {
					return e
				}
				if e = writeEvent(out, event, c.Wrap); e != nil {
					return fmt.Errorf("write sign-in: %w", e)
				}
				return s.mark(key, id, now)
			})
			if e != nil {
				return fmt.Errorf("collect Graph %s: %w", kind, e)
			}
			if e = s.advance(key, until); e != nil {
				return e
			}
			start = until
		}
	}
	return s.prune(now)
}
