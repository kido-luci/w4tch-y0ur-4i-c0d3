package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublisherPublish(t *testing.T) {
	t.Run("success sends header, path and envelope", func(t *testing.T) {
		var gotPath, gotSecret string
		var gotBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", r.Method)
			}
			gotPath = r.URL.Path
			gotSecret = r.Header.Get("X-Design-Ingest")
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		p := &Publisher{api: srv.URL, site: "https://cowork.example.com", secret: "s3cret", client: srv.Client()}
		scene := []byte(`{"type":"excalidraw","elements":[]}`)
		if err := p.Publish("abc123", "checkout flow", scene); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if gotPath != "/designs/abc123" {
			t.Errorf("path = %s", gotPath)
		}
		if gotSecret != "s3cret" {
			t.Errorf("secret header = %q", gotSecret)
		}
		var envelope struct {
			Name  string          `json:"name"`
			Scene json.RawMessage `json:"scene"`
		}
		if err := json.Unmarshal(gotBody, &envelope); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if envelope.Name != "checkout flow" || string(envelope.Scene) != string(scene) {
			t.Errorf("envelope = %+v", envelope)
		}
		if got := p.ReviewURL("abc123"); got != "https://cowork.example.com/#/d/abc123" {
			t.Errorf("review url = %s", got)
		}
	})

	t.Run("no secret -> errPublishNotConfigured", func(t *testing.T) {
		t.Setenv("DESIGN_INGEST_SECRET", "")
		p := newPublisherFromEnv()
		err := p.Publish("abc", "x", []byte(`{}`))
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("err = %v, want not-configured", err)
		}
	})

	t.Run("backend error surfaces status and body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer srv.Close()
		p := &Publisher{api: srv.URL, site: "x", secret: "s", client: srv.Client()}
		err := p.Publish("abc", "x", []byte(`{}`))
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err = %v, want 403 mention", err)
		}
	})
}

func TestMarkPublished(t *testing.T) {
	ds := NewDrawingStore(newTestDataDB(t))
	d, err := ds.Create("login screen", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := ds.MarkPublished(d.ID, time.Time{}); err == nil {
		t.Error("zero base must be rejected")
	}
	if _, err := ds.MarkPublished("nope", d.UpdatedAt); err == nil {
		t.Error("unknown id must be rejected")
	}

	out, err := ds.MarkPublished(d.ID, d.UpdatedAt)
	if err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if !out.PublishedAt.Equal(d.UpdatedAt) {
		t.Errorf("PublishedAt = %v, want %v", out.PublishedAt, d.UpdatedAt)
	}

	// A later content write bumps UpdatedAt — the published copy goes stale
	// without MarkPublished being touched (the ThumbUpdatedAt idiom).
	time.Sleep(2 * time.Millisecond)
	upd, err := ds.SetContent(d.ID, []byte(`{"elements":[1]}`), time.Time{})
	if err != nil {
		t.Fatalf("set content: %v", err)
	}
	if upd.PublishedAt.Equal(upd.UpdatedAt) {
		t.Error("publish must go stale after a content write")
	}
}
