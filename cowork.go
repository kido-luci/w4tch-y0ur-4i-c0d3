package main

// Publishing a drawing pushes its .excalidraw scene to a review backend's
// /designs/{id} endpoint, where allowlisted teammates open it read-only and
// comment. This is the app's only true network egress (the summarize button
// shells out to the local `claude` CLI): one PUT per explicit publish click,
// nothing automatic. Auth is a shared secret, sent as X-Design-Ingest.
//
// There is no built-in backend: every endpoint comes from the environment, so
// an install that sets nothing can send nothing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// errPublishNotConfigured means COWORK_API or DESIGN_INGEST_SECRET is unset —
// publishing is opt-in, so a fresh install fails soft with instructions rather
// than a broken button, and has nowhere to send to until you name a backend.
var errPublishNotConfigured = errors.New("publishing not configured: set COWORK_API and DESIGN_INGEST_SECRET (and optionally COWORK_URL for review links)")

// Publisher pushes scenes to the review backend. Zero value is unusable; build
// with newPublisherFromEnv.
type Publisher struct {
	api    string // backend origin, no trailing slash
	site   string // cowork viewer origin, for review links
	secret string
	client *http.Client
}

// newPublisherFromEnv reads COWORK_API / COWORK_URL / DESIGN_INGEST_SECRET.
// There is no default backend: an unset COWORK_API leaves api empty, which
// Publish reports as not-configured. COWORK_URL is only the viewer origin for
// review links; unset, it falls back to the API origin. The secret is read live
// per publish (not cached here) so the launchd environment can be fixed without
// rebuilding mental state about ordering.
func newPublisherFromEnv() *Publisher {
	api := strings.TrimRight(strings.TrimSpace(os.Getenv("COWORK_API")), "/")
	site := strings.TrimRight(strings.TrimSpace(os.Getenv("COWORK_URL")), "/")
	if site == "" {
		site = api
	}
	return &Publisher{api: api, site: site, client: &http.Client{Timeout: 15 * time.Second}}
}

// ReviewURL is where teammates open the published drawing.
func (p *Publisher) ReviewURL(id string) string {
	return p.site + "/#/d/" + id
}

// Publish PUTs one drawing's scene to the backend. The scene bytes are sent
// verbatim inside the JSON envelope the backend expects ({name, scene}).
func (p *Publisher) Publish(id, name string, scene []byte) error {
	secret := strings.TrimSpace(os.Getenv("DESIGN_INGEST_SECRET"))
	if p.secret != "" { // tests inject directly
		secret = p.secret
	}
	// No backend and no secret are the same failure to the user: nothing to
	// send to, or nothing to authenticate with — either way, don't send.
	if p.api == "" || secret == "" {
		return errPublishNotConfigured
	}
	body, err := json.Marshal(struct {
		Name  string          `json:"name"`
		Scene json.RawMessage `json:"scene"`
	}{Name: name, Scene: scene})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, p.api+"/designs/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Design-Ingest", secret)
	res, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("publish: backend answered %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
