package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeCloud serves one handler and records the last request.
type fakeCloud struct {
	t       *testing.T
	srv     *httptest.Server
	method  string
	path    string
	headers http.Header
	body    map[string]any
	raw     []byte
}

func newFakeCloud(t *testing.T, status int, response string, headers map[string]string) (*fakeCloud, *Client) {
	t.Helper()
	f := &fakeCloud{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.method, f.path, f.headers = r.Method, r.URL.Path, r.Header.Clone()
		f.raw, _ = io.ReadAll(r.Body)
		f.body = nil
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(f.raw, &f.body)
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(f.srv.Close)
	c, err := New(Options{APIKey: "dtk_live_k", APISecret: "dts_s", BaseURL: f.srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestNewRequiresCredentials(t *testing.T) {
	t.Setenv("DTELECOM_API_KEY", "")
	t.Setenv("DTELECOM_API_SECRET", "")
	t.Setenv("DTELECOM_CLOUD_URL", "")
	if _, err := New(Options{BaseURL: "https://x"}); err == nil || !strings.Contains(err.Error(), "APIKey") {
		t.Fatalf("want missing key error, got %v", err)
	}
	if _, err := New(Options{APIKey: "k", APISecret: "s"}); err == nil || !strings.Contains(err.Error(), "BaseURL") {
		t.Fatalf("want missing base URL error, got %v", err)
	}
	t.Setenv("DTELECOM_API_KEY", "k")
	t.Setenv("DTELECOM_API_SECRET", "s")
	t.Setenv("DTELECOM_CLOUD_URL", "https://cloud.example/")
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.baseURL != "https://cloud.example" || c.http.authorization != "Bearer k:s" {
		t.Fatalf("env not used: %q %q", c.http.baseURL, c.http.authorization)
	}
}

func TestDurationMinutes(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int
		ok   bool
	}{
		{90 * time.Second, 2, true},
		{time.Second, 1, true},
		{30 * time.Minute, 30, true},
		{24 * time.Hour, 1440, true},
		{24*time.Hour + time.Second, 0, false},
		{0, 0, false},
		{-time.Minute, 0, false},
	}
	for _, c := range cases {
		got, err := durationMinutes(c.d)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("durationMinutes(%s) = %d, %v; want %d, ok=%v", c.d, got, err, c.want, c.ok)
		}
	}
}

func TestCreateToken(t *testing.T) {
	f, c := newFakeCloud(t, 200, `{"session_id":"s1","token":"jwt","ws_url":"wss://n1","expires_at":"2026-11-17T10:00:00Z"}`, nil)
	tok, err := c.CreateToken(context.Background(), TokenOptions{
		Room: "demo", Identity: "alice", Name: "Alice", ClientIP: "1.2.3.4",
		WebhookURLs: []string{"https://hook"},
		Grants:      &Grants{CanSubscribe: Bool(false), RoomAdmin: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.method != "POST" || f.path != "/v1/webrtc/token" || f.headers.Get("Authorization") != "Bearer dtk_live_k:dts_s" {
		t.Fatalf("request %s %s %q", f.method, f.path, f.headers.Get("Authorization"))
	}
	if f.body["room_name"] != "demo" || f.body["identity"] != "alice" || f.body["name"] != "Alice" ||
		f.body["duration_minutes"] != float64(360) || f.body["client_ip"] != "1.2.3.4" {
		t.Fatalf("body %v", f.body)
	}
	if _, ok := f.body["metadata"]; ok {
		t.Fatal("empty metadata sent")
	}
	grants := f.body["grants"].(map[string]any)
	if len(grants) != 2 || grants["can_subscribe"] != false || grants["room_admin"] != true {
		t.Fatalf("grants %v", grants)
	}
	if tok.Token != "jwt" || tok.WsURL != "wss://n1" || tok.SessionID != "s1" || !tok.ExpiresAt.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("token %+v", tok)
	}
}

func TestCreateTokenRoundsTTL(t *testing.T) {
	f, c := newFakeCloud(t, 200, `{}`, nil)
	if _, err := c.CreateToken(context.Background(), TokenOptions{Room: "r", Identity: "i", TTL: 90 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if f.body["duration_minutes"] != float64(2) {
		t.Fatalf("duration %v", f.body["duration_minutes"])
	}
	if _, err := c.CreateToken(context.Background(), TokenOptions{Room: "r", Identity: "i", TTL: 25 * time.Hour}); err == nil {
		t.Fatal("25h accepted")
	}
}

func TestExtendAndAdminToken(t *testing.T) {
	f, c := newFakeCloud(t, 200, `{"token":"t2","ws_url":"wss://n1","new_expires_at":"2026-11-17T11:00:00Z"}`, nil)
	ext, err := c.ExtendToken(context.Background(), "s1", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/webrtc/token/extend" || f.body["session_id"] != "s1" || f.body["additional_minutes"] != float64(30) || ext.Token != "t2" || ext.ExpiresAt.IsZero() {
		t.Fatalf("extend %s %v %+v", f.path, f.body, ext)
	}

	f2, c2 := newFakeCloud(t, 200, `{"token":"adm","api_urls":null,"expires_at":"2026-11-17T11:00:00Z"}`, nil)
	adm, err := c2.AdminToken(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if f2.path != "/v1/webrtc/admin-token" || f2.body["room_name"] != "demo" || adm.APIURLs == nil || len(adm.APIURLs) != 0 {
		t.Fatalf("admin %s %v %+v", f2.path, f2.body, adm)
	}
}

func TestAPIError(t *testing.T) {
	_, c := newFakeCloud(t, 422, `{"error":"backend_rejected","id":"job1","detail":{"code":"UNSUPPORTED_TAG","position":13}}`, map[string]string{"Retry-After": "1"})
	_, err := c.ExtendToken(context.Background(), "s1", time.Minute)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T %v", err, err)
	}
	if apiErr.Status != 422 || apiErr.Code != "backend_rejected" || apiErr.ID != "job1" ||
		apiErr.Detail["code"] != "UNSUPPORTED_TAG" || apiErr.RetryAfter != time.Second {
		t.Fatalf("error %+v", apiErr)
	}
	if !IsCode(err, "backend_rejected") || IsCode(err, "other") {
		t.Fatal("IsCode")
	}
	if err.Error() != "dtelecom cloud: 422 backend_rejected" {
		t.Fatalf("message %q", err.Error())
	}

	_, c2 := newFakeCloud(t, 502, `<html>bad gateway</html>`, nil)
	_, err = c2.ExtendToken(context.Background(), "s1", time.Minute)
	if apiErr, ok := err.(*APIError); !ok || apiErr.Code != "http_502" || apiErr.RetryAfter != 0 {
		t.Fatalf("non-JSON error %+v", err)
	}
}
