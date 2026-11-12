package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func fixture(t *testing.T) (Config, *API, *State, *int) {
	t.Helper()
	c := Defaults()
	c.TenantID = "11111111-1111-1111-1111-111111111111"
	c.ClientID = c.TenantID
	c.ContentTypes = []string{"Audit.General"}
	c.StatePath = filepath.Join(t.TempDir(), "state.db")
	downloads := new(int)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if e := r.ParseForm(); e != nil {
				t.Error(e)
			}
			if r.Form.Get("client_secret") != "secret" {
				t.Error("missing secret")
			}
			io.WriteString(w, `{"access_token":"token","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing bearer token")
		}
		switch r.URL.Path {
		case "/feed/subscriptions/content":
			if r.URL.Query().Get("page") == "2" {
				io.WriteString(w, `[]`)
				return
			}
			w.Header().Set("NextPageUri", server.URL+"/feed/subscriptions/content?page=2")
			_ = json.NewEncoder(w).Encode([]Blob{{ID: "blob1", URI: server.URL + "/feed/blob"}})
		case "/feed/blob":
			*downloads++
			io.WriteString(w, `[{"Id":"one","n":9007199254740993},{"Id":"two"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	a := NewAPI(c, "secret")
	a.base = server.URL + "/feed/"
	a.tokenURL = server.URL + "/token"
	s, e := OpenState(c.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return c, a, s, downloads
}
func TestPollRestart(t *testing.T) {
	c, a, s, n := fixture(t)
	now := time.Now()
	var out bytes.Buffer
	if e := Poll(context.Background(), c, a, s, &out, now); e != nil {
		t.Fatal(e)
	}
	if *n != 1 || strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), "9007199254740993") {
		t.Fatalf("bad output %s", out.String())
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := OpenState(c.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	out.Reset()
	if e = Poll(context.Background(), c, a, reopened, &out, now.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if *n != 1 || out.Len() != 0 {
		t.Fatal("completed blob replayed after restart")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func TestOutputFailureDoesNotCheckpoint(t *testing.T) {
	c, a, s, n := fixture(t)
	now := time.Now()
	if Poll(context.Background(), c, a, s, brokenWriter{}, now) == nil {
		t.Fatal("expected output error")
	}
	cursor, e := s.cursor(scope(c.TenantID, "Audit.General"))
	if e != nil || cursor.After(time.Now().Add(-c.Lag-c.Lookback+time.Second)) {
		t.Fatal("advanced cursor on failure")
	}
	var out bytes.Buffer
	if e = Poll(context.Background(), c, a, s, &out, now); e != nil {
		t.Fatal(e)
	}
	if *n != 2 {
		t.Fatal("failed blob not retried")
	}
}
func TestStateLock(t *testing.T) {
	c, _, _, _ := fixture(t)
	if s, e := OpenState(c.StatePath); e == nil {
		s.Close()
		t.Fatal("concurrent state access accepted")
	}
}
func TestStaleCheckpoint(t *testing.T) {
	c, a, s, _ := fixture(t)
	now := time.Now()
	if e := s.advance(scope(c.TenantID, "Audit.General"), now.Add(-8*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if e := Poll(context.Background(), c, a, s, io.Discard, now); e == nil || !strings.Contains(e.Error(), "retention") {
		t.Fatalf("expected retention error: %v", e)
	}
}
func TestUntrustedURL(t *testing.T) {
	a := NewAPI(Defaults(), "")
	for _, u := range []string{"https://evil.example/feed", "http://manage.office.com/api/v1.0/", "https://manage.office.com@evil.example/"} {
		if a.trusted(u) == nil {
			t.Fatalf("trusted %s", u)
		}
	}
}
func TestRetryAndRefresh(t *testing.T) {
	for _, status := range []int{429, 500, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			attempts := 0
			tokens := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens++
					io.WriteString(w, `{"access_token":"token","expires_in":3600}`)
					return
				}
				attempts++
				if attempts == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, `[]`)
			}))
			defer server.Close()
			a := NewAPI(Defaults(), "secret")
			a.base = server.URL + "/"
			a.tokenURL = server.URL + "/token"
			if _, _, e := a.request(context.Background(), "GET", server.URL+"/content", nil, true); e != nil {
				t.Fatal(e)
			}
			if attempts != 2 {
				t.Fatal(attempts)
			}
			if status == 401 && tokens != 2 {
				t.Fatal("token not refreshed")
			}
		})
	}
}
func TestConfigAndAge(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(file, []byte("tenant_id: from-file\nlookback: 2h\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("M365_TENANT_ID", "from-env")
	c, e := Load(file)
	if e != nil || c.TenantID != "from-env" || c.Lookback != 2*time.Hour {
		t.Fatalf("config %v %v", c, e)
	}
	if e = os.WriteFile(file, []byte("client_secret: forbidden\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Load(file); e == nil {
		t.Fatal("plaintext secret accepted")
	}
	identity, e := age.GenerateX25519Identity()
	if e != nil {
		t.Fatal(e)
	}
	c.IdentityFile = filepath.Join(dir, "key")
	c.SecretFile = filepath.Join(dir, "secret.age")
	if e = os.WriteFile(c.IdentityFile, []byte(identity.String()), 0600); e != nil {
		t.Fatal(e)
	}
	var encrypted bytes.Buffer
	w, e := age.Encrypt(&encrypted, identity.Recipient())
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.WriteString(w, "encrypted-secret\n")
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(c.SecretFile, encrypted.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	secret, e := c.Secret()
	if e != nil || secret != "encrypted-secret" {
		t.Fatalf("decrypt failed %v", e)
	}
}
func TestPaginationFailurePreservesCompletedBlobs(t *testing.T) {
	c, a, s, _ := fixture(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/feed/blob" {
			io.WriteString(w, `[{"Id":"ok"}]`)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("NextPageUri", a.base+"subscriptions/content?page=2")
		_ = json.NewEncoder(w).Encode([]Blob{{ID: "first", URI: a.base + "blob"}})
	}))
	defer server.Close()
	a.base = server.URL + "/feed/"
	a.token = "token"
	a.expires = time.Now().Add(time.Hour)
	if e := Poll(context.Background(), c, a, s, io.Discard, time.Now()); e == nil {
		t.Fatal("expected pagination error")
	}
	done, e := s.done(scope(c.TenantID, "Audit.General"), "first")
	if e != nil || !done {
		t.Fatal("completed blob not saved")
	}
	cursor, e := s.cursor(scope(c.TenantID, "Audit.General"))
	if e != nil || cursor.After(time.Now().Add(-c.Lag-c.Lookback+time.Second)) {
		t.Fatal("window advanced before final page")
	}
	if requests != 3 {
		t.Fatal(requests)
	}
}

func TestWindowsAndInitialFailure(t *testing.T) {
	c, a, s, _ := fixture(t)
	c.Lookback = 49 * time.Hour
	c.Overlap = 0
	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(-c.Lag)
	start := end.Add(-c.Lookback)
	var ranges [][2]time.Time
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, e := time.Parse("2006-01-02T15:04:05", r.URL.Query().Get("startTime"))
		if e != nil {
			t.Error(e)
		}
		to, e := time.Parse("2006-01-02T15:04:05", r.URL.Query().Get("endTime"))
		if e != nil {
			t.Error(e)
		}
		ranges = append(ranges, [2]time.Time{from, to})
		if fail {
			w.WriteHeader(400)
			return
		}
		io.WriteString(w, `[]`)
	}))
	defer server.Close()
	a.base = server.URL + "/feed/"
	a.token = "token"
	a.expires = time.Now().Add(time.Hour)
	if e := Poll(context.Background(), c, a, s, io.Discard, now); e == nil {
		t.Fatal("expected initial failure")
	}
	cursor, e := s.cursor(scope(c.TenantID, "Audit.General"))
	if e != nil || !cursor.Equal(start) {
		t.Fatal("initial lower bound not preserved", cursor, e)
	}
	fail = false
	ranges = nil
	if e = Poll(context.Background(), c, a, s, io.Discard, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if len(ranges) != 3 || !ranges[0][0].Equal(start) || !ranges[2][1].Equal(end.Add(time.Hour)) {
		t.Fatal(ranges)
	}
	for i, r := range ranges {
		if r[1].Sub(r[0]) > 24*time.Hour {
			t.Fatal("oversized API window")
		}
		if i > 0 && !ranges[i-1][1].Equal(r[0]) {
			t.Fatal("gap between windows")
		}
	}
}

func TestSubscribeOnlyMissing(t *testing.T) {
	starts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed/subscriptions/list":
			io.WriteString(w, `[{"contentType":"Audit.General","status":"enabled"}]`)
		case "/feed/subscriptions/start":
			starts++
			if r.Method != "POST" || r.URL.Query().Get("contentType") != "Audit.Exchange" {
				t.Error("unexpected start")
			}
			io.WriteString(w, `{}`)
		default:
			t.Error("unexpected request")
		}
	}))
	defer server.Close()
	a := NewAPI(Defaults(), "")
	a.base = server.URL + "/feed/"
	a.token = "token"
	a.expires = time.Now().Add(time.Hour)
	if e := a.Subscribe(context.Background(), []string{"Audit.General", "Audit.Exchange"}); e != nil {
		t.Fatal(e)
	}
	if starts != 1 {
		t.Fatal(starts)
	}
}

func TestMalformedResponses(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[{"contentId":"missing-url"}]`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer server.Close()
			a := NewAPI(Defaults(), "")
			a.base = server.URL + "/"
			a.token = "token"
			a.expires = time.Now().Add(time.Hour)
			if e := a.List(context.Background(), "Audit.General", time.Now().Add(-time.Hour), time.Now(), func(Blob) error { return nil }); e == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestPaginationCycle(t *testing.T) {
	var a *API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("NextPageUri", a.base+"page")
		io.WriteString(w, `[]`)
	}))
	defer server.Close()
	a = NewAPI(Defaults(), "")
	a.base = server.URL + "/"
	a.token = "token"
	a.expires = time.Now().Add(time.Hour)
	if e := a.List(context.Background(), "Audit.General", time.Now().Add(-time.Hour), time.Now(), func(Blob) error { return nil }); e == nil || !strings.Contains(e.Error(), "cycle") {
		t.Fatal(e)
	}
}
