package httpapi

// Design library routes: the .excalidraw scenes in data.db, their thumbnails,
// backups and cowork publishing. Split out of api.go's Register, which had
// grown to 1512 lines with every route in one function.
//
// The parameters are named for the locals they replaced, so the handler bodies
// moved here verbatim. That is the point: a rename sweep across sixty handler
// bodies is where a refactor like this goes wrong, and the compiler cannot see
// a body that captured the wrong-but-same-typed variable.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"watch-your-ai-code/internal/board"
	"watch-your-ai-code/internal/cowork"
	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/sse"

	"github.com/go-chi/chi/v5"
)

func routeDrawings(
	router chi.Router,
	drawings *board.DrawingStore,
	todos *board.TodoStore,
	hub *sse.Hub,
) {
	// Built here rather than in the composition root: publishing is
	// self-contained (env-configured, no shared state) and only the design
	// routes use it — keep the surface local to them.
	publisher := cowork.NewPublisherFromEnv()

	router.Get("/api/drawings", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, drawings.List())
	})

	router.Post("/api/drawings", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := drawings.Create(in.Name, in.Group)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	router.Get("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := drawings.Content(chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(content)
	})

	// Scene saves can be big (pasted images arrive as data URLs in `files`),
	// so this endpoint gets its own generous cap instead of the 1MB one.
	// X-Base-Updated-At (the updatedAt the client last saw, RFC3339Nano) makes
	// the write conditional: a stale base gets 409 instead of clobbering a
	// save that happened elsewhere (another tab, an MCP client).
	router.Put("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "scene too large (20MB max)")
			return
		}
		var base time.Time
		if h := r.Header.Get("X-Base-Updated-At"); h != "" {
			if base, err = time.Parse(time.RFC3339Nano, h); err != nil {
				httpx.WriteJSONError(w, http.StatusBadRequest, "bad X-Base-Updated-At (want RFC3339)")
				return
			}
		}
		d, err := drawings.SetContent(chi.URLParam(r, "id"), content, base)
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, board.ErrDrawingConflict) {
			httpx.WriteJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	router.Patch("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Pointers so a metadata edit can carry any field: name renames,
		// group moves the drawing to a tab, topics replaces its tag set (and
		// ""/[] are real values — back to Ungrouped / untagged — distinct
		// from "not provided").
		var in struct {
			Name   *string   `json:"name"`
			Group  *string   `json:"group"`
			Topics *[]string `json:"topics"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		// The steps apply in sequence, so a failure midway can leave earlier
		// fields already persisted — track whether ANY step landed and
		// broadcast for it even on the error path, or other tabs would stay
		// stale about a change that did happen.
		applied := false
		d, err := drawings.Get(id)
		if err == nil && in.Name != nil {
			if d, err = drawings.Rename(id, *in.Name); err == nil {
				applied = true
			}
		}
		if err == nil && in.Group != nil {
			if d, err = drawings.SetGroup(id, *in.Group); err == nil {
				applied = true
			}
		}
		if err == nil && in.Topics != nil {
			if d, err = drawings.SetTopics(id, *in.Topics); err == nil {
				applied = true
			}
		}
		if applied {
			hub.Broadcast("drawings-updated", drawings.List())
		}
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		httpx.WriteJSON(w, d)
	})

	router.Post("/api/drawings/{id}/duplicate", func(w http.ResponseWriter, r *http.Request) {
		d, err := drawings.Duplicate(chi.URLParam(r, "id"))
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	// Publish is an explicit user action: push the current scene to the review
	// backend, then stamp PublishedAt with the version that was sent (the
	// ThumbUpdatedAt freshness idiom — edits after publish show as stale).
	router.Post("/api/drawings/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		d, err := drawings.Get(id)
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		content, err := drawings.Content(id)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err := publisher.Publish(id, d.Name, content); err != nil {
			if errors.Is(err, cowork.ErrPublishNotConfigured) {
				httpx.WriteJSONError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			// The backend, not this app, is unreachable/unhappy — 502 keeps
			// that distinction visible in the UI's error message.
			httpx.WriteJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		out, err := drawings.MarkPublished(id, d.UpdatedAt)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, struct {
			board.Drawing
			ReviewURL string `json:"reviewUrl"`
		}{out, publisher.ReviewURL(id)})
	})

	// Thumbnails are rendered client-side (the browser is the only place the
	// Excalidraw renderer exists) and cached here. A GET misses (404) until a
	// thumbnail rendered from the CURRENT scene version has been uploaded —
	// the grid regenerates on miss, so MCP writes self-heal on the next view.
	router.Get("/api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		b, err := drawings.Thumbnail(chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	router.Put("/api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		base, err := time.Parse(time.RFC3339Nano, r.Header.Get("X-Base-Updated-At"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "X-Base-Updated-At is required (the scene updatedAt the thumbnail was rendered from)")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "thumbnail too large (4MB max)")
			return
		}
		d, err := drawings.SetThumbnail(chi.URLParam(r, "id"), data, base)
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	router.Delete("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := drawings.Delete(id); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		// Cards must not keep pointing at a drawing that no longer exists.
		if todos.UnlinkDrawing(id) {
			hub.Broadcast("todos-updated", todos.List())
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
