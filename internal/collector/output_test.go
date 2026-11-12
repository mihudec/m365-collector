package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteEvent(t *testing.T) {
	original := json.RawMessage(`{"id":"one","large":9007199254740993,"nested":{"value":null},"text":"<>&","array":[true,2]}`)
	for _, wrap := range []string{"", "entra", "literal.key", "quote\"slash\\newline\n"} {
		t.Run(wrap, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeEvent(&out, original, wrap); err != nil {
				t.Fatal(err)
			}
			if strings.Count(out.String(), "\n") != 1 {
				t.Fatal("output must be one NDJSON line")
			}
			result := bytes.TrimSpace(out.Bytes())
			if wrap != "" {
				var object map[string]json.RawMessage
				if err := json.Unmarshal(result, &object); err != nil {
					t.Fatal(err)
				}
				if len(object) != 1 {
					t.Fatal("extra wrapper fields")
				}
				result = object[wrap]
			}
			if !bytes.Equal(result, original) {
				t.Fatalf("event changed: %s", result)
			}
		})
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestWriteEventFailure(t *testing.T) {
	if err := writeEvent(shortWriter{}, json.RawMessage(`{}`), "entra"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeEvent(&out, json.RawMessage(`invalid`), "entra"); err == nil || out.Len() != 0 {
		t.Fatal("invalid JSON emitted")
	}
}

func TestWrappedActivityPoll(t *testing.T) {
	c, a, s, _ := fixture(t)
	c.Wrap = "office365"
	now := time.Now()
	if err := Poll(context.Background(), c, a, s, brokenWriter{}, now); err == nil {
		t.Fatal("expected write failure")
	}
	var out bytes.Buffer
	if err := Poll(context.Background(), c, a, s, &out, now); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatal(err)
		}
		if len(obj) != 1 || obj[c.Wrap] == nil {
			t.Fatal("missing wrapper")
		}
	}
}
func TestWrappedGraphPoll(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, a, s := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "one", "createdDateTime": now.Add(-30 * time.Minute).Format(time.RFC3339)}}})
	})
	c.Wrap = "entra"
	c.Overlap = 0
	if err := PollGraph(context.Background(), c, a, s, shortWriter{}, now); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := PollGraph(context.Background(), c, a, s, &out, now); err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj) != 1 || obj[c.Wrap] == nil {
		t.Fatal("missing wrapper")
	}
}
func TestLoadWrap(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("wrap: entra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if c.Wrap != "entra" {
		t.Fatal("wrapper not loaded")
	}
	if Defaults().Wrap != "" {
		t.Fatal("wrapping must be opt-in")
	}
}
