# AGENTS.md

rssbreeze: a terminal RSS reader (Go, Bubble Tea TUI) with a feed overview, on-demand feed loading, bookmarks, seen-tracking, date filters, and feed import/export.

## Commands

```bash
go build -o rssbreeze .          # build (binary is gitignored)
go run .                         # run
./rssbreeze version              # version info, handled before Bubble Tea starts
./rssbreeze export [file]        # print/write feeds.json; handled before Bubble Tea
./rssbreeze import <file|->      # merge feeds into feeds.json; handled before Bubble Tea
go test ./...                    # main_test.go, stdlib only (testing/quick for properties)
go vet ./...

# Build with version info, as CI does
CGO_ENABLED=0 go build -ldflags "-X main.version=v0.0.0 -X main.commit=dev -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o rssbreeze .
```

- No linter config, no Makefile.
- When running the TUI by hand, set `XDG_CACHE_HOME` to a temp dir so your real `feeds.json`/`seen.json` stay untouched.
- `main.go` is **not** gofmt-clean: `gofmt -l .` flags the column-aligned `:=` style block in `itemDelegate.Render`. Don't reformat the whole file as a side effect of an unrelated change.
- Module path is `github.com/grammeaway/rssbreeze/v2`. The `/v2` suffix is required for `go install ...@latest`.
- `go.mod` declares `go 1.26.1`, but CI's `setup-go` pins `1.24` and depends on GOTOOLCHAIN auto-download. Bump both if you change either.

## Architecture

Everything is in `main.go` (~1200 lines), using Bubble Tea's Model-Update-View pattern. Direct deps are only `bubbles`, `bubbletea`, `lipgloss`, and `charmbracelet/x/ansi` (width-safe truncation). Feeds are parsed with stdlib `encoding/xml` into `FeedDoc`, which covers RSS (`channel > item`) and Atom (`entry`, mapped onto `Item` by `AtomEntry.item()`).

Other top-level files: `main_test.go`, `docs/preview.png` (README screenshot), `docs/plan-overview-and-lazy-loading.md` (design notes for the overview/lazy-loading rework), and `.github/workflows/release.yml`.

**Core types:**
- `model`: all app state. `view` is `overviewView` or `itemsView`. `scope` is the feed shown in the items view (`""` = all feeds).
- `feedState` (`model.states`, keyed by feed name): items, `loaded`, `loading`, `err` for this session. A missing entry means "not loaded".
- `NewsItem`: a parsed entry, tagged with `FeedName`. `IsNew` and `IsBookmarked` are derived in `scopeItems()` on every rebuild and never stored.
- `Feed` / `FeedsConfig`: configured feeds plus the optional `page_size`, stored in `feeds.json`.
- `Config`: seen GUIDs (`LastSeen map[string]bool`), stored in `seen.json`.
- `BookmarkEntry` / `BookmarksFile`: bookmarks keyed by GUID, stored in `bookmarks.json`.
- `itemDelegate`: renders each item as exactly three lines: title with `●`/`★` markers, description, then `[feed] date`. Each line is collapsed with `oneLine` and truncated to the list width. The overview uses `list.DefaultDelegate` with `overviewItem`.

**Layout invariant:** `View()` renders exactly one body (overview, items list, help, or a modal) plus a one-line footer, and both lists are sized `height - 1`. Bubble Tea drops lines from the **top** when a view outgrows the terminal, so anything taller hides the list title. That was the "missing header" bug. Keep every rendered row single-line; `TestItemRendersExactlyThreeLinesWithinWidth` guards this.

**Fetching:** nothing is fetched at startup. `openScope` → `fetchScope` fires one `startFetch` command per feed that isn't loaded yet, and each returns a `feedFetchedMsg`. `r` uses `force=true`. Results that arrive for a feed that no longer matches `m.feeds` (edited or deleted in the meantime) are dropped via `hasFeed`.

**Key handling order in `Update`:** edit modal (`editing`, `editIndex` -1 = add, `editStep` 0 = name, 1 = URL), date-filter modal (`filtering`), delete confirmation (`confirmDelete`), help (any key closes it), `q`/`ctrl+c`/`h`, then `updateOverview` or `updateItems`. `esc` cancels a modal, goes from the items view back to the overview, and quits from the overview.

**Filtering and paging:** `itemMatchesFilters` is the single predicate (bookmarks-only, `filterDays`). `applyFilters` rebuilds the items list and its title from `scopeItems()`. In the blended scope it caps the list at `blendLimit`, which `updateItems` grows by `pageSize()` when the cursor reaches the last item. Built-in list fuzzy filtering is disabled.

## Gotchas

- **Fetching:** `httpClient` has a 15s timeout, and non-200 responses are errors. Errors are shown in the overview row (`feedStatus`). Keep stderr writes out of the TUI path: they land in the alt-screen and garble it.
- **Unknown feed formats parse silently to 0 items:** `encoding/xml` doesn't check the root element, so anything that is neither RSS nor Atom yields no items and no error.
- **Identity:**
  - Items are keyed by GUID. When `GUID` is empty, `Link` is used instead. Both seen-state and bookmarks depend on that key.
  - Feeds are identified by **name**, not URL. Add/edit (`nameTaken`) and import (`mergeFeeds`) reject duplicate names. Legacy duplicates are still deleted together. Any edit drops that feed's `feedState`.
- **Dates:** `parseRSSDate` tries 6 formats (`time.RFC3339` covers Atom, fractional seconds included), then silently falls back to `time.Now()`. Items with unparseable dates therefore sort to the top.
- **HTML and URLs:**
  - `stripHTML` is a naive `<`/`>` state machine. It doesn't decode entities.
  - `sanitizeURL` repairs hosts that absorbed part of the path, e.g. `example.compath` → `example.com/path`. It only checks a fixed TLD list and runs when a URL is opened, not when feeds are fetched.
  - `openURL` uses xdg-open, rundll32, or open depending on the OS. On any other `GOOS` it silently does nothing.
- **Persistence:**
  - All files live in `os.UserCacheDir()/rssbreeze/`.
  - Write errors are mostly swallowed in the TUI. `saveFeeds` returns its error, but only the import CLI checks it.
  - `readFeeds` is the strict loader, used by export/import so a corrupt `feeds.json` is never overwritten. `loadFeeds` swallows errors.
  - `loadConfig` creates the directory, and `saveConfig` relies on it already existing.
- **Save timing (invariants to preserve):**
  - `seen.json` is written only by `saveConfig()`, which runs on quit (`q`/`esc`/`ctrl+c`) and inside `markAllAsSeen`. Individual `markAsSeen` calls (`Enter`) are not saved on their own, so a crash loses that session's per-item seen state.
  - `saveConfig()` calls `cleanupConfig()` first, which prunes `LastSeen` down to the GUIDs of loaded items. It does this **only when every configured feed loaded successfully this session**. Otherwise, with lazy loading, it would wipe the seen state of feeds that were never opened.
  - `markAllAsSeen` (`n`) only marks items that match the active filters (per-feed clearing).
  - `bookmarks.json` and `feeds.json` are written immediately on every change.
- **Docs:** Keybindings are listed both in the in-app `helpText` const and in the README controls tables. Update both together.

## CI / release

`.github/workflows/release.yml` runs on `v*` tag pushes. It cross-compiles these targets:
- linux/{amd64,arm64,riscv64} and windows/amd64 on ubuntu, with `CGO_ENABLED=0`
- darwin/{amd64,arm64} on macos, where CGO is not explicitly disabled

Version metadata is injected through ldflags. Each build is packaged as a tarball with LICENSE and README, then published as a GitHub release with `SHA256SUMS`. No CI runs on PRs or pushes.
