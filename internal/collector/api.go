package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type API struct {
	client                                  *http.Client
	base, tokenURL, clientID, secret, token string
	oauthScope                              string
	expires                                 time.Time
	publisher                               string
}

func NewAPI(c Config, secret string) *API {
	return &API{client: &http.Client{Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: "https://manage.office.com/api/v1.0/" + c.TenantID + "/activity/feed/", tokenURL: "https://login.microsoftonline.com/" + c.TenantID + "/oauth2/v2.0/token", clientID: c.ClientID, secret: secret, publisher: c.TenantID, oauthScope: "https://manage.office.com/.default"}
}
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (a *API) authenticate(ctx context.Context) error {
	if a.token != "" && time.Now().Before(a.expires) {
		return nil
	}
	form := url.Values{"client_id": {a.clientID}, "client_secret": {a.secret}, "grant_type": {"client_credentials"}, "scope": {a.oauthScope}}
	b, _, err := a.request(ctx, http.MethodPost, a.tokenURL, []byte(form.Encode()), false)
	if err != nil {
		return err
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(b, &t) != nil || t.AccessToken == "" || t.ExpiresIn <= 0 {
		return errors.New("invalid token response")
	}
	a.token = t.AccessToken
	a.expires = time.Now().Add(time.Duration(t.ExpiresIn)*time.Second - time.Minute)
	return nil
}
func (a *API) trusted(raw string) error {
	u, e := url.Parse(raw)
	base, _ := url.Parse(a.base)
	if e != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || !strings.HasPrefix(u.Path, base.Path) {
		return errors.New("API returned an untrusted content or pagination URL")
	}
	return nil
}
func (a *API) request(ctx context.Context, method, raw string, body []byte, auth bool) ([]byte, http.Header, error) {
	if auth {
		if err := a.trusted(raw); err != nil {
			return nil, nil, err
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		if auth {
			if err := a.authenticate(ctx); err != nil {
				return nil, nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, raw, bytes.NewReader(body))
		if err != nil {
			return nil, nil, errors.New("invalid request URL")
		}
		if auth {
			req.Header.Set("Authorization", "Bearer "+a.token)
		} else {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Accept", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, errors.New("HTTP transport failed")
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, nil, errors.New("reading HTTP response failed")
		}
		if len(b) > 64*1024*1024 {
			return nil, nil, errors.New("HTTP response exceeds 64 MiB")
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return b, resp.Header, nil
		}
		if resp.StatusCode == 401 && auth && attempt == 0 {
			a.token = ""
			continue
		}
		if resp.StatusCode != 429 && resp.StatusCode < 500 {
			return nil, nil, fmt.Errorf("API request failed (HTTP %d)", resp.StatusCode)
		}
		if attempt == 4 {
			return nil, nil, fmt.Errorf("API retries exhausted (HTTP %d)", resp.StatusCode)
		}
		delay := time.Second * time.Duration(1<<attempt)
		if v := resp.Header.Get("Retry-After"); v != "" {
			if n, e := strconv.Atoi(v); e == nil && n >= 0 {
				delay = time.Duration(n) * time.Second
			} else if t, e := http.ParseTime(v); e == nil {
				delay = time.Until(t)
			}
		}
		if delay < 0 {
			delay = 0
		}
		if delay > 5*time.Minute {
			return nil, nil, errors.New("server retry delay exceeds 5 minutes; retry next invocation")
		}
		if err = wait(ctx, delay); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, errors.New("request retries exhausted")
}
func (a *API) endpoint(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("PublisherIdentifier", a.publisher)
	return a.base + path + "?" + q.Encode()
}
func (a *API) Subscribe(ctx context.Context, types []string) error {
	b, _, e := a.request(ctx, "GET", a.endpoint("subscriptions/list", nil), nil, true)
	if e != nil {
		return e
	}
	var existing []struct {
		ContentType string `json:"contentType"`
		Status      string `json:"status"`
	}
	if e = json.Unmarshal(b, &existing); e != nil {
		return errors.New("invalid subscription response")
	}
	for _, kind := range types {
		enabled := false
		for _, s := range existing {
			if s.ContentType == kind && s.Status == "enabled" {
				enabled = true
			}
		}
		if !enabled {
			_, _, e = a.request(ctx, "POST", a.endpoint("subscriptions/start", url.Values{"contentType": {kind}}), nil, true)
			if e != nil {
				return e
			}
		}
	}
	return nil
}

type Blob struct {
	ID  string `json:"contentId"`
	URI string `json:"contentUri"`
}

func (a *API) List(ctx context.Context, kind string, start, end time.Time, consume func(Blob) error) error {
	next := a.endpoint("subscriptions/content", url.Values{"contentType": {kind}, "startTime": {start.UTC().Format("2006-01-02T15:04:05")}, "endTime": {end.UTC().Format("2006-01-02T15:04:05")}})
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return errors.New("pagination cycle detected")
		}
		seen[next] = true
		b, h, e := a.request(ctx, "GET", next, nil, true)
		if e != nil {
			return e
		}
		var blobs []Blob
		if json.Unmarshal(b, &blobs) != nil || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return errors.New("invalid content listing")
		}
		for _, blob := range blobs {
			if blob.ID == "" || blob.URI == "" {
				return errors.New("incomplete content descriptor")
			}
			if e = consume(blob); e != nil {
				return e
			}
		}
		next = h.Get("NextPageUri")
	}
	return nil
}
func (a *API) Events(ctx context.Context, blob Blob) ([]json.RawMessage, error) {
	b, _, e := a.request(ctx, "GET", blob.URI, nil, true)
	if e != nil {
		return nil, e
	}
	var events []json.RawMessage
	if json.Unmarshal(b, &events) != nil || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, errors.New("invalid event array")
	}
	for _, event := range events {
		if len(event) == 0 || event[0] != '{' {
			return nil, errors.New("event must be a JSON object")
		}
	}
	return events, nil
}
