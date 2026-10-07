# AGENTS.md

rssbreeze: a terminal RSS reader (Go, Bubble Tea TUI) with multi-feed support, bookmarks, seen-tracking, and feed/date filters.

## Commands

```bash
go build -o rssbreeze .          # build (binary is gitignored)
go run .                         # run
./rssbreeze version              # version info, handled before Bubble Tea starts
go vet ./...                     # only static check available; passes today

# Build with version info, as CI does
CGO_ENABLED=0 go build -ldflags "-X main.version=v0.0.0 -X main.commit=dev -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o rssbreeze .
```

- There are no tests, no linter config, no Makefile.
- `main.go` is **not** gofmt-clean: `gofmt -l .` flags the column-aligned `:=` blocks. Don't reformat the whole file as a side effect of an unrelated change.
- Module path is `github.com/grammeaway/rssbreeze/v2`. The `/v2` suffix is required for `go install ...@latest`.
- `go.mod` declares `go 1.26.1`, but CI's `setup-go` pins `1.24` and depends on GOTOOLCHAIN auto-download. Bump both if you change either.

## Architecture

Everything is in `main.go` (~900 lines), using Bubble Tea's Model-Update-View pattern. Direct deps are only `bubbles`, `bubbletea`, and `lipgloss`. RSS is parsed with stdlib `encoding/xml` (`RSS > channel > item`). **Atom feeds are not supported.**

Other top-level files: `docs/preview.png` (README screenshot) and `.github/workflows/release.yml`.

**Core types:**
- `model`: all app state (list, items, feeds, config, bookmarks, filters, modal state).
- `NewsItem`: a parsed entry, tagged with `FeedName`.
- `Feed` / `FeedsConfig`: configured feeds, stored in `feeds.json`.
- `Config`: seen GUIDs (`LastSeen map[string]bool`), stored in `seen.json`.
- `BookmarkEntry` / `BookmarksFile`: bookmarks keyed by GUID, stored in `bookmarks.json`.
- `itemDelegate`: renders each item on three lines (title with `●`/`★` markers, description truncated to 80 bytes, then `[feed] date`).

**Messages:** `fetchedMsg` carries the merged `[]NewsItem` back to the model. `errMsg` is handled in `Update`, but nothing ever sends one, so the `m.err` view is effectively dead code.

**Key handling order in `Update`:** add-feed modal first (`addingFeed`, two steps via `addFeedStep` 0 = name, 1 = URL), then the date-filter modal (`filtering`), then global keys. Inside a modal, `esc` cancels the modal; outside one, it quits.

**Filtering:** `itemMatchesFilters` is the single predicate (bookmarks-only, `filterDays`, `filterFeed`). `applyFilters` rebuilds the list and title from it. Built-in list fuzzy filtering is disabled. `F` cycles `filterFeed` through `""` → each feed name → `""`. `D` deletes the feed selected by the filter and refetches.

## Gotchas

- **Fetching:** `fetchAllFeeds` runs one goroutine per feed, merges the results, and sorts by `PubDate` desc.
  - A feed that fails is skipped: the error goes to stderr and the rest of the fetch continues.
  - `http.Get` has no timeout, so one hung feed blocks the whole fetch.
  - stderr writes land in the alt-screen and can garble the TUI.
- **Identity:**
  - Items are keyed by GUID. When `GUID` is empty, `Link` is used instead. Both seen-state and bookmarks depend on that key.
  - Feeds are identified by **name**, not URL. Duplicate names get filtered and deleted together.
- **Dates:** `parseRSSDate` tries 7 formats, then falls back to `time.Now()` with a stderr warning. Items with unparseable dates therefore sort to the top.
- **HTML and URLs:**
  - `stripHTML` is a naive `<`/`>` state machine. It doesn't decode entities.
  - `sanitizeURL` repairs hosts that absorbed part of the path, e.g. `example.compath` → `example.com/path`. It only checks a fixed TLD list and runs when a URL is opened, not when feeds are fetched.
  - `openURL` uses xdg-open, rundll32, or open depending on the OS. On any other `GOOS` it silently does nothing.
- **Persistence:**
  - All files live in `os.UserCacheDir()/rssbreeze/`.
  - Write errors are mostly swallowed silently. Only bookmarks report to stderr.
  - `loadConfig` creates the directory, and `saveConfig` relies on it already existing.
- **Save timing (invariants to preserve):**
  - `seen.json` is written only by `saveConfig()`, which runs on quit (`q`/`esc`/`ctrl+c`) and inside `markAllAsSeen`. Individual `markAsSeen` calls (`Enter`) are not saved on their own, so a crash loses that session's per-item seen state.
  - `saveConfig()` calls `cleanupConfig()` first, which prunes `LastSeen` down to GUIDs present in `m.items`. If the user quits before the fetch completes (`m.items` empty), **`seen.json` is wiped**. Keep this in mind when changing the quit or fetch paths.
  - `markAllAsSeen` (`n`) only marks items that match the active filters (per-feed clearing).
  - `bookmarks.json` and `feeds.json` are written immediately on every change.
- **Docs:** Keybindings are listed both in the in-app help string in `View()` and in the README controls table. Update both together.

## CI / release

`.github/workflows/release.yml` runs on `v*` tag pushes. It cross-compiles these targets:
- linux/{amd64,arm64,riscv64} and windows/amd64 on ubuntu, with `CGO_ENABLED=0`
- darwin/{amd64,arm64} on macos, where CGO is not explicitly disabled

Version metadata is injected through ldflags. Each build is packaged as a tarball with LICENSE and README, then published as a GitHub release with `SHA256SUMS`. No CI runs on PRs or pushes.
