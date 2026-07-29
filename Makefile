# Build the single-binary viewer: frontend first, then embed into Go.
# frontend/ and backend/ are siblings; `npm run build` writes the bundle into
# backend/internal/web/dist, which is what go:embed picks up — go:embed cannot
# reach a parent directory, so the artifact crosses over, not the source.
build:
	cd frontend && npm install && npm run build
	cd backend && go build -o ../watch-your-ai-code .

test:
	cd backend && go test ./...

run: build
	./watch-your-ai-code

# --- release gate --------------------------------------------------------------
# `check` is the local stand-in for the (disabled) CI workflow: the same five
# gates, plus one CI never had — the embed gate, comparing the asset hash inside
# the freshly built binary against the built bundle on disk. That mismatch is the
# "new and stale at once" failure that bit v0.41.0 (see CLAUDE.md). Actions is
# off since 2026-07-17 (quota); CI returns 2026-08-01, Release stays local.
# Both gates run through scripts/wyac-ship, which drops one JSON record per
# run (exit, duration, log tail — pass or fail) into ~/.wyac/ships for the
# viewer's ship history. The viewer being down loses nothing: files wait.
check:
	@scripts/wyac-ship watch-your-ai-code check - $(MAKE) check-run

check-run:
	cd frontend && npm ci && npm run build && npm test
	@unformatted=$$(cd backend && gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	cd backend && go vet ./...
	cd backend && go test ./...
	cd backend && go build -o ../watch-your-ai-code .
	@served=$$(grep -a -oE 'assets/index-[A-Za-z0-9_-]+\.js' watch-your-ai-code | head -1); \
	disk=$$(grep -oE 'assets/index-[A-Za-z0-9_-]+\.js' backend/internal/web/dist/index.html | head -1); \
	if [ -z "$$disk" ] || [ "$$served" != "$$disk" ]; then \
		echo "embed gate FAILED: binary embeds '$$served', the built bundle has '$$disk'"; \
		exit 1; fi
	@echo "check: all gates green"

# `make release VERSION=v0.43.0` — fail-fast guards, full check, then the same
# four platforms release.yml used to build, tag push, gh release. The guards run
# before check on purpose: a missing CHANGELOG entry should fail in seconds, not
# after a five-minute build. dist/ is wiped of old tarballs before the build so
# the `dist/*.tar.gz` upload glob carries only this version's four binaries —
# without the wipe it accumulated across every release (156 tarballs by v0.79.0)
# and the upload eventually timed the release out and left it a draft.
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

release-guards:
	@[ -n "$(VERSION)" ] || { echo "usage: make release VERSION=vX.Y.Z"; exit 1; }
	@case "$(VERSION)" in v*) ;; *) echo "VERSION must start with 'v', got '$(VERSION)'"; exit 1;; esac
	@[ -z "$$(git status --porcelain --untracked-files=no)" ] || { \
		echo "working tree dirty — commit first:"; \
		git status --short --untracked-files=no; exit 1; }
	@grep -q "^## $(VERSION) " CHANGELOG.md || \
		{ echo "CHANGELOG.md has no '## $(VERSION)' entry"; exit 1; }

release:
	@scripts/wyac-ship watch-your-ai-code release "$(VERSION)" $(MAKE) release-run VERSION="$(VERSION)"

release-run: release-guards check-run
	@set -e; mkdir -p dist; \
	rm -f dist/*.tar.gz; \
	for platform in $(PLATFORMS); do \
		goos=$${platform%/*}; goarch=$${platform#*/}; \
		name="watch-your-ai-code_$(VERSION)_$${goos}_$${goarch}"; \
		echo "building $$name"; \
		GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 \
				go build -C backend -trimpath -ldflags "-s -w" -o "../dist/$$name" .; \
		tar -czf "dist/$$name.tar.gz" -C dist "$$name"; \
		rm "dist/$$name"; \
	done
	git tag "$(VERSION)" 2>/dev/null || echo "tag $(VERSION) already exists locally, reusing"
	git push origin "$(VERSION)"
	@if gh release view "$(VERSION)" >/dev/null 2>&1; then \
		gh release upload "$(VERSION)" dist/*.tar.gz --clobber; \
	else \
		gh release create "$(VERSION)" --title "$(VERSION)" --generate-notes dist/*.tar.gz; \
	fi

# --- dev loop -----------------------------------------------------------------
# Two servers, neither of them the one on 4777. Vite serves the frontend with
# HMR and proxies /api to the dev binary on DEV_ADDR, so a .ts/.css edit shows
# up without `make build` — that path embeds the built bundle, this one bypasses it.
#
# The dev binary keeps its own board + design library under .dev/config. The
# launchd instance on 4777 is writing the real todos.json, and two binaries on
# one file is how a field gets silently dropped. Transcripts it reads for real:
# they're read-only, and dev against an empty index would show nothing.
#
# Needs air once: go install github.com/air-verse/air@latest
DEV_ADDR ?= 127.0.0.1:4778
DEV_CONFIG ?= $(CURDIR)/.dev/config

dev-api:
	air -- -addr $(DEV_ADDR) -config-dir $(DEV_CONFIG)

dev-web:
	cd frontend && npm install && npm run dev

# Both in one terminal; Ctrl+C stops both (the trap kills the process group).
dev:
	@trap 'kill 0' EXIT INT TERM; \
	$(MAKE) dev-api & \
	$(MAKE) dev-web & \
	wait

.PHONY: build test run check check-run release-guards release release-run dev dev-api dev-web
