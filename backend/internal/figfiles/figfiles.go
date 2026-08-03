// Package figfiles lists the design documents a scope's repos carry —
// `.fig` / `.pen` files under `<repo>/design/` — and opens one in the
// OpenPencil desktop app.
//
// It is the one place in the server that launches something on the user's
// machine. Everything else here is a read-only view, so the whitelist below is
// not a formality: Open re-lists the scope and refuses any path that is not in
// the result, the same guard every git drill-down applies to its `?repo`. An
// arbitrary path is an error, never an argument to `open`.
//
// Like git and codegraph, this takes the resolved repos rather than the
// resolver, so the listing and the guard are testable without an index.
package figfiles

import (
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/repos"
)

// designDir is the per-repo folder scanned for documents. A convention rather
// than a setting: it is what makes the listing scope itself: the scope already
// resolves to repo roots, so `<root>/design` needs no second source of truth.
const designDir = "design"

// File is one design document. Root/Folder mirror repos.Repo so the client can
// group by repo and label the group the way the git tab labels it.
type File struct {
	Root       string    `json:"root"`
	Folder     string    `json:"folder"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

var (
	// ErrUnknownFile means the path is not one of the scope's design files.
	ErrUnknownFile = errors.New("unknown design file for this scope")
	// ErrUnsupported means this platform has no `open`. The release
	// cross-compiles four platforms; only darwin can launch the app.
	ErrUnsupported = errors.New("opening design files is only supported on macOS")
)

// isDoc reports whether name looks like a document OpenPencil can open.
func isDoc(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".fig", ".pen":
		return true
	}
	return false
}

// List returns the design documents in each repo's design/ directory, newest
// first. One level deep only — a recursive walk would have to learn which
// directories to skip, and a design folder deep enough to need one is not a
// case this has. Repos without the folder simply contribute nothing.
func List(rs []repos.Repo) []File {
	out := []File{}
	for _, repo := range rs {
		dir := filepath.Join(repo.Root, designDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A repo without design/ contributes nothing, silently. Any other
			// error must reach the log: fd exhaustion (EMFILE) once made every
			// scope answer an empty library, indistinguishable from the normal
			// case until the process was restarted.
			if !errors.Is(err, fs.ErrNotExist) {
				log.Printf("figfiles: list %s: %v", dir, err)
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isDoc(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, File{
				Root:       repo.Root,
				Folder:     repo.Folder,
				Name:       e.Name(),
				Path:       filepath.Join(dir, e.Name()),
				Size:       info.Size(),
				ModifiedAt: info.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModifiedAt.After(out[j].ModifiedAt) })
	return out
}

// allowed reports whether path is one of files. This is the security boundary:
// the path handed to `open` must be one the scope itself produced.
func allowed(files []File, path string) bool {
	for _, f := range files {
		if f.Path == path {
			return true
		}
	}
	return false
}

// Open launches path in the OpenPencil desktop app.
//
// The whitelist check runs before the platform check on purpose: an unknown
// path must answer the same on every OS, so the test that pins the guard runs
// on CI's Linux runner too and not only on a Mac.
func Open(rs []repos.Repo, path string) error {
	if !allowed(List(rs), path) {
		return ErrUnknownFile
	}
	if runtime.GOOS != "darwin" {
		return ErrUnsupported
	}
	// Absolute path for the same reason internal/github pins `gh`: launchd's
	// PATH is not the shell's. /usr/bin/open is part of the OS.
	//
	// `open`'s own stderr is the useful half of the failure — "Unable to find
	// application named 'OpenPencil'" is actionable where a bare "exit status 1"
	// reaches the user as noise, and this error is rendered verbatim in the UI.
	out, err := exec.Command("/usr/bin/open", "-a", "OpenPencil", path).CombinedOutput()
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(string(out)); detail != "" {
		return fmt.Errorf("could not launch OpenPencil: %s", detail)
	}
	return fmt.Errorf("could not launch OpenPencil: %w", err)
}

func Register(router chi.Router, rr *repos.Resolver) {
	router.Get("/api/design-files", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, struct {
			Files []File `json:"files"`
		}{Files: List(rr.Bound(r.URL.Query().Get("scope")))})
	})

	router.Post("/api/design-files/open", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		switch err := Open(rr.Bound(r.URL.Query().Get("scope")), path); {
		case errors.Is(err, ErrUnknownFile):
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrUnsupported):
			httpx.WriteJSONError(w, http.StatusNotImplemented, err.Error())
		case err != nil:
			// `open` failing usually means OpenPencil is not installed.
			httpx.WriteJSONError(w, http.StatusBadGateway, err.Error())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}
