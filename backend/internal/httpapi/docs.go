package httpapi

// Docs-wiki routes. Split out of api.go's Register — see drawings.go for why
// the parameters are named for the locals they replaced.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"watch-your-ai-code/internal/board"
	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/sse"

	"github.com/go-chi/chi/v5"
)

func routeDocs(router chi.Router, docs *board.DocStore, todos *board.TodoStore, hub *sse.Hub) {
	// --- docs wiki (data.db; the server is the single writer). Mutations
	// broadcast the fresh metadata list so other tabs' trees stay in sync; the
	// page body is fetched per page and is last-writer-wins with optimistic
	// concurrency, exactly like the design library's scenes.

	router.Get("/api/docs", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, docs.List())
	})

	router.Post("/api/docs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title, ParentID, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Create(in.Title, in.ParentID, in.Group)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		httpx.WriteJSON(w, d)
	})

	router.Get("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := docs.Get(chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		body, _ := docs.Content(d.ID)
		httpx.WriteJSON(w, docWithBody{Doc: d, Body: body})
	})

	// Body saves. X-Base-Updated-At (the updatedAt the client last saw,
	// RFC3339Nano) makes the write conditional: a stale base gets 409 instead
	// of clobbering a save that happened elsewhere (another tab, an MCP client).
	router.Put("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "body too large (4MB max)")
			return
		}
		var base time.Time
		if h := r.Header.Get("X-Base-Updated-At"); h != "" {
			if base, err = time.Parse(time.RFC3339Nano, h); err != nil {
				httpx.WriteJSONError(w, http.StatusBadRequest, "bad X-Base-Updated-At (want RFC3339)")
				return
			}
		}
		d, err := docs.SetContent(chi.URLParam(r, "id"), string(body), base)
		if errors.Is(err, board.ErrDocNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, board.ErrDocConflict) {
			httpx.WriteJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		httpx.WriteJSON(w, d)
	})

	router.Patch("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.DocPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Update(chi.URLParam(r, "id"), p)
		if errors.Is(err, board.ErrDocNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, board.ErrDocCycle) {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		httpx.WriteJSON(w, d)
	})

	router.Delete("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := docs.Delete(id); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		// Cards must not keep pointing at a doc that no longer exists.
		if todos.UnlinkDoc(id) {
			hub.Broadcast("todos-updated", todos.List())
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
