package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func graphFixture(t *testing.T, handler http.HandlerFunc) (Config, *API, *State) {
	t.Helper()
	c := Defaults()
	c.Source = "graph"
	c.TenantID = "11111111-1111-1111-1111-111111111111"
	c.ClientID = c.TenantID
	c.StatePath = filepath.Join(t.TempDir(), "state.db")
	c.Lookback = time.Hour
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a := NewGraphAPI(c, "secret")
	a.base = server.URL + "/v1.0/auditLogs/"
	a.token = "token"
	a.expires = time.Now().Add(time.Hour)
	s, e := OpenState(c.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return c, a, s
}
func TestGraphPaginationRestartAndIsolation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-30 * time.Minute).Format(time.RFC3339)
	var a *API
	pages := 0
	c, api, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing authorization")
		}
		if r.URL.Query().Get("$skiptoken") != "opaque+value" {
			if r.URL.Query().Get("$top") != "1000" || !strings.Contains(r.URL.Query().Get("$filter"), "createdDateTime lt") {
				t.Error("missing window filter")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "one", "createdDateTime": created, "status": map[string]any{"errorCode": 0}}}, "@odata.nextLink": a.base + "signIns?$skiptoken=opaque%2Bvalue"})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "two", "createdDateTime": created, "status": map[string]any{"errorCode": 50126}}}})
		}
	})
	a = api
	// An Activity API completion with the same identifier must not suppress Graph.
	if e := s.mark(scope(c.TenantID, "Audit.AzureActiveDirectory"), "one", now); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e := PollGraph(context.Background(), c, a, s, &out, now); e != nil {
		t.Fatal(e)
	}
	if pages != 2 || strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), "50126") {
		t.Fatalf("bad output %s", out.String())
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	s2, e := OpenState(c.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	defer s2.Close()
	out.Reset()
	if e = PollGraph(context.Background(), c, a, s2, &out, now); e != nil {
		t.Fatal(e)
	}
	if out.Len() != 0 {
		t.Fatal("replayed completed events")
	}
}
func TestGraphFailureRecovery(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-30 * time.Minute).Format(time.RFC3339)
	var a *API
	failPage := true
	c, api, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			if failPage {
				w.WriteHeader(403)
				io.WriteString(w, "sensitive response")
				return
			}
			io.WriteString(w, `{"value":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "one", "createdDateTime": created}}, "@odata.nextLink": a.base + "signIns?page=2"})
	})
	a = api
	c.Overlap = 0
	key := scope(c.TenantID, "graph:v1.0:interactiveUser")
	if e := PollGraph(context.Background(), c, a, s, brokenWriter{}, now); e == nil {
		t.Fatal("expected write failure")
	}
	done, e := s.done(key, "one")
	if e != nil || done {
		t.Fatal("checkpointed failed output")
	}
	var out bytes.Buffer
	e = PollGraph(context.Background(), c, a, s, &out, now)
	if e == nil || strings.Contains(e.Error(), "sensitive") {
		t.Fatal("expected sanitized pagination failure", e)
	}
	cursor, e := s.cursor(key)
	if e != nil || !cursor.Equal(now.Add(-c.Lag-c.Lookback)) {
		t.Fatal("cursor advanced on incomplete page", cursor, e)
	}
	failPage = false
	out.Reset()
	if e = PollGraph(context.Background(), c, a, s, &out, now); e != nil {
		t.Fatal(e)
	}
	if out.Len() != 0 {
		t.Fatal("successful event replayed")
	}
}
func TestGraphTokenScope(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form.Get("scope")
		io.WriteString(w, `{"access_token":"graph-token","expires_in":3600}`)
	}))
	defer server.Close()
	a := NewGraphAPI(Defaults(), "secret")
	a.tokenURL = server.URL
	if e := a.authenticate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if got != "https://graph.microsoft.com/.default" {
		t.Fatal(got)
	}
}
func TestGraphBetaFilters(t *testing.T) {
	for _, kind := range []string{"interactiveUser", "nonInteractiveUser", "servicePrincipal", "managedIdentity"} {
		t.Run(kind, func(t *testing.T) {
			c, a, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Query().Get("$filter"), "signInEventTypes/any(t: t eq '"+kind+"')") {
					t.Error("missing category filter")
				}
				io.WriteString(w, `{"value":[]}`)
			})
			c.GraphVersion = "beta"
			c.SignInTypes = []string{kind}
			if e := c.Validate(); e != nil {
				t.Fatal(e)
			}
			if e := PollGraph(context.Background(), c, a, s, io.Discard, time.Now()); e != nil {
				t.Fatal(e)
			}
			c.GraphVersion = "v1.0"
			if kind != "interactiveUser" && c.Validate() == nil {
				t.Fatal("accepted beta type for stable API")
			}
		})
	}
}
func TestGraphMalformedAndUnsafePages(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"value":null}`, `{"value":{}}`, `{"value":[{}]}`, `{"value":[{"id":"one","createdDateTime":"invalid"}]}`, `{"value":[],"@odata.nextLink":"https://evil.example/v1.0/auditLogs/signIns"}`, `{"value":[],"@odata.nextLink":123}`} {
		t.Run(body, func(t *testing.T) {
			c, a, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if e := PollGraph(context.Background(), c, a, s, io.Discard, time.Now()); e == nil {
				t.Fatal("accepted invalid page")
			}
		})
	}
}
func TestGraphRetentionAndPruning(t *testing.T) {
	c, a, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") })
	now := time.Now()
	key := scope(c.TenantID, "graph:v1.0:interactiveUser")
	if e := s.advance(key, now.Add(-31*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if e := PollGraph(context.Background(), c, a, s, io.Discard, now); e == nil {
		t.Fatal("accepted expired cursor")
	}
	for _, entry := range []struct {
		id   string
		days int
	}{{"keep", 29}, {"prune", 32}} {
		if e := s.mark(key, entry.id, now.Add(-time.Duration(entry.days)*24*time.Hour)); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.prune(now); e != nil {
		t.Fatal(e)
	}
	keep, _ := s.done(key, "keep")
	prune, _ := s.done(key, "prune")
	if !keep || prune {
		t.Fatal("wrong Graph dedup retention")
	}
}

func TestGraphHourlyWindowsAndLateArrival(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	late := false
	var starts, ends []time.Time
	c, a, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.URL.Query().Get("$filter"))
		if len(fields) != 7 {
			t.Fatal("unexpected filter")
		}
		start, e := time.Parse(time.RFC3339, fields[2])
		if e != nil {
			t.Error(e)
		}
		end, e := time.Parse(time.RFC3339, fields[6])
		if e != nil {
			t.Error(e)
		}
		starts = append(starts, start)
		ends = append(ends, end)
		if late {
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "late", "createdDateTime": start.Add(time.Minute).Format(time.RFC3339)}}})
		} else {
			io.WriteString(w, `{"value":[]}`)
		}
	})
	c.Lookback = 3 * time.Hour
	var out bytes.Buffer
	if e := PollGraph(context.Background(), c, a, s, &out, now); e != nil {
		t.Fatal(e)
	}
	if len(starts) != 3 {
		t.Fatal(starts)
	}
	for i := range starts {
		if ends[i].Sub(starts[i]) != time.Hour {
			t.Fatal("non-hour window")
		}
		if i > 0 && !starts[i].Equal(ends[i-1]) {
			t.Fatal("window gap")
		}
	}
	late = true
	starts = nil
	ends = nil
	if e := PollGraph(context.Background(), c, a, s, &out, now); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "late") {
		t.Fatal("late arrival missed")
	}
	if len(starts) != 1 || !starts[0].Equal(now.Add(-c.Lag-c.Overlap)) {
		t.Fatal("incorrect overlap")
	}
}
func TestGraphPaginationCycle(t *testing.T) {
	var a *API
	c, api, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "@odata.nextLink": a.base + "signIns?page=2"})
	})
	a = api
	if e := PollGraph(context.Background(), c, a, s, io.Discard, time.Now()); e == nil || !strings.Contains(e.Error(), "cycle") {
		t.Fatal(e)
	}
}
