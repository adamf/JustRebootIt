package grafana

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewRequiresAllFields(t *testing.T) {
	if New("", "u", "p") != nil || New("http://x", "", "p") != nil || New("http://x", "u", "") != nil {
		t.Error("New should return nil when base/user/pass are not all set")
	}
	if New("http://x", "u", "p") == nil {
		t.Error("New should return a client when all fields are set")
	}
}

func TestNilClientPostIsNoop(t *testing.T) {
	var c *Client
	if err := c.Post(context.Background(), Annotation{}); err != nil {
		t.Errorf("nil-client Post should be a no-op, got %v", err)
	}
}

func TestPostSendsAnnotation(t *testing.T) {
	var gotUser, gotPass, gotText string
	var gotTags []any
	var gotTimeMillis int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/annotations" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotUser, gotPass, _ = r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		gotText, _ = payload["text"].(string)
		gotTags, _ = payload["tags"].([]any)
		if f, ok := payload["time"].(float64); ok {
			gotTimeMillis = int64(f)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "secret")
	when := time.Date(2026, 6, 10, 4, 0, 0, 0, time.UTC)
	err := c.Post(context.Background(), Annotation{
		Time: when,
		Tags: []string{"justrebootit", "event:7"},
		Text: "Event #7 — congestion",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotUser != "admin" || gotPass != "secret" {
		t.Errorf("basic auth = %q/%q, want admin/secret", gotUser, gotPass)
	}
	if gotText != "Event #7 — congestion" {
		t.Errorf("text = %q", gotText)
	}
	if len(gotTags) != 2 {
		t.Errorf("tags = %v, want 2", gotTags)
	}
	if gotTimeMillis != when.UnixMilli() {
		t.Errorf("time = %d ms, want %d", gotTimeMillis, when.UnixMilli())
	}
}

func TestPostSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid auth"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "wrong")
	if err := c.Post(context.Background(), Annotation{Time: time.Now()}); err == nil {
		t.Error("expected an error on 401")
	}
}
