package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func testModel(feeds ...Feed) model {
	return newModel(
		FeedsConfig{Feeds: feeds},
		Config{LastSeen: map[string]bool{}},
		BookmarksFile{Bookmarks: map[string]BookmarkEntry{}},
	)
}

// A list item that renders taller or wider than its slot pushes the list title off-screen.
func TestItemRendersExactlyThreeLinesWithinWidth(t *testing.T) {
	check := func(title, desc, feed string, width uint8, isNew, bookmarked bool) bool {
		w := int(width)%200 + 10
		l := list.New(nil, itemDelegate{}, w, 50)
		item := NewsItem{Title: title, Description: desc, FeedName: feed, PubDate: time.Now(), IsNew: isNew, IsBookmarked: bookmarked}
		for _, index := range []int{0, 1} { // selected, unselected
			var buf bytes.Buffer
			itemDelegate{}.Render(&buf, l, index, item)
			lines := strings.Split(buf.String(), "\n")
			if len(lines) != 3 {
				t.Logf("width %d: got %d lines: %q", w, len(lines), buf.String())
				return false
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > w {
					t.Logf("width %d: line too wide (%d): %q", w, ansi.StringWidth(line), line)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
	// The arXiv case that triggered the bug.
	if !check("Title", "arXiv:2610.04861v1 Announce Type: new \nAbstract: Real-time", "Research: Linux, OS", 80, true, false) {
		t.Error("arXiv-style description broke the layout")
	}
	if !check(strings.Repeat("日本語 ", 50), "<p>a</p>\n\t<p>b</p>", "feed\nname", 20, true, true) {
		t.Error("wide/multiline text broke the layout")
	}
}

func feedsFrom(seed []uint8) []Feed {
	names := []string{"a", "b", "c", " a ", ""}
	urls := []string{"u1", "u2", "u3", "u1 ", ""}
	var feeds []Feed
	for i := 0; i+1 < len(seed); i += 2 {
		feeds = append(feeds, Feed{Name: names[int(seed[i])%len(names)], URL: urls[int(seed[i+1])%len(urls)]})
	}
	return feeds
}

func TestMergeFeedsIsIdempotentAndUnique(t *testing.T) {
	check := func(localSeed, incomingSeed []uint8, pageSize int) bool {
		local, _, _ := mergeFeeds(FeedsConfig{PageSize: pageSize}, feedsFrom(localSeed))
		incoming := feedsFrom(incomingSeed)
		once, _, _ := mergeFeeds(local, incoming)
		twice, added, _ := mergeFeeds(once, incoming)
		if !reflect.DeepEqual(once, twice) || added != 0 || once.PageSize != pageSize {
			return false
		}
		names, urls := map[string]bool{}, map[string]bool{}
		for _, f := range once.Feeds {
			if f.Name == "" || f.URL == "" || names[f.Name] || urls[f.URL] {
				return false
			}
			names[f.Name], urls[f.URL] = true, true
		}
		return true
	}
	if err := quick.Check(check, nil); err != nil {
		t.Error(err)
	}
}

func TestMergeFeedsWarnsOnNameClash(t *testing.T) {
	local := FeedsConfig{Feeds: []Feed{{"HN", "https://a"}}}
	merged, added, warnings := mergeFeeds(local, []Feed{{"HN", "https://b"}, {"Other", "https://a"}, {"New", "https://c"}})
	if added != 1 || len(warnings) != 1 || len(merged.Feeds) != 2 || merged.Feeds[1].Name != "New" {
		t.Errorf("added=%d warnings=%v feeds=%v", added, warnings, merged.Feeds)
	}
	if len(local.Feeds) != 1 {
		t.Error("mergeFeeds mutated its input")
	}
}

func TestParseFeed(t *testing.T) {
	body := `<rss><channel>
		<item><title>One</title><link>https://x/1</link><guid>g1</guid><pubDate>Mon, 06 Oct 2025 10:00:00 GMT</pubDate></item>
		<item><title>Two</title><link>https://x/2</link></item>
	</channel></rss>`
	items, err := parseFeed([]byte(body), "X")
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if items[0].GUID != "g1" || items[1].GUID != "https://x/2" || items[0].FeedName != "X" || items[0].PubDate.Year() != 2025 {
		t.Errorf("unexpected items: %+v", items)
	}
}

// Trimmed arXiv API response.
func TestParseAtomFeed(t *testing.T) {
	body := `<?xml version='1.0' encoding='UTF-8'?>
	<feed xmlns:arxiv="http://arxiv.org/schemas/atom" xmlns="http://www.w3.org/2005/Atom">
	  <id>https://arxiv.org/api/xyz</id>
	  <title>arXiv Query</title>
	  <link href="https://arxiv.org/api/query" type="application/atom+xml"/>
	  <entry>
	    <id>http://arxiv.org/abs/2610.05009v1</id>
	    <title>Rethinking Tool Design</title>
	    <updated>2026-10-05T07:04:41Z</updated>
	    <link href="https://arxiv.org/pdf/2610.05009v1" rel="related" type="application/pdf"/>
	    <link href="https://arxiv.org/abs/2610.05009v1" rel="alternate" type="text/html"/>
	    <summary>LLM agents for RCA.</summary>
	    <published>2026-10-04T07:04:41Z</published>
	  </entry>
	  <entry>
	    <id>urn:2</id>
	    <title>Second</title>
	    <updated>2026-10-03T01:02:03.456+02:00</updated>
	    <link href="https://example.com/2"/>
	    <content type="html">&lt;p&gt;Body&lt;/p&gt;</content>
	  </entry>
	</feed>`
	items, err := parseFeed([]byte(body), "arXiv")
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	first, second := items[0], items[1]
	if first.Title != "Rethinking Tool Design" || first.GUID != "http://arxiv.org/abs/2610.05009v1" ||
		first.Link != "https://arxiv.org/abs/2610.05009v1" || first.Description != "LLM agents for RCA." ||
		!first.PubDate.Equal(time.Date(2026, 10, 4, 7, 4, 41, 0, time.UTC)) || first.FeedName != "arXiv" {
		t.Errorf("first entry: %+v", first)
	}
	if second.Link != "https://example.com/2" || second.Description != "<p>Body</p>" ||
		!second.PubDate.Equal(time.Date(2026, 10, 2, 23, 2, 3, 456e6, time.UTC)) {
		t.Errorf("second entry: %+v", second)
	}
}

func TestCleanupConfigOnlyPrunesAfterFullLoad(t *testing.T) {
	m := testModel(Feed{"a", "u1"}, Feed{"b", "u2"})
	m.config.LastSeen = map[string]bool{"keep": true, "stale": true}
	m.states["a"] = feedState{loaded: true, items: []NewsItem{{GUID: "keep"}}}

	m.cleanupConfig()
	if len(m.config.LastSeen) != 2 {
		t.Fatalf("pruned on partial load: %v", m.config.LastSeen)
	}

	m.states["b"] = feedState{loaded: true, err: errFake}
	m.cleanupConfig()
	if len(m.config.LastSeen) != 2 {
		t.Fatalf("pruned while a feed errored: %v", m.config.LastSeen)
	}

	m.states["b"] = feedState{loaded: true}
	m.cleanupConfig()
	if !reflect.DeepEqual(m.config.LastSeen, map[string]bool{"keep": true}) {
		t.Fatalf("expected only 'keep', got %v", m.config.LastSeen)
	}
}

var errFake = errors.New("fake")

func TestBlendedViewIsNewestFirstAndPaged(t *testing.T) {
	m := testModel(Feed{"a", "u1"}, Feed{"b", "u2"})
	day := func(d int) time.Time { return time.Date(2025, 1, d, 0, 0, 0, 0, time.UTC) }
	m.states["a"] = feedState{loaded: true, items: []NewsItem{{GUID: "a1", PubDate: day(1)}, {GUID: "a3", PubDate: day(3)}}}
	m.states["b"] = feedState{loaded: true, items: []NewsItem{{GUID: "b2", PubDate: day(2)}, {GUID: "b4", PubDate: day(4)}}}
	m.blendLimit = 3
	m.applyFilters()

	var got []string
	for _, it := range m.list.Items() {
		got = append(got, it.(NewsItem).GUID)
	}
	if !reflect.DeepEqual(got, []string{"b4", "a3", "b2"}) || m.matchCount != 4 {
		t.Errorf("got %v (matchCount %d)", got, m.matchCount)
	}
}

func TestEditRejectsDuplicateNames(t *testing.T) {
	for _, index := range []int{-1, 1} { // add, edit
		m := testModel(Feed{"a", "u1"}, Feed{"b", "u2"})
		m.startEdit(index)
		m.feedNameInput.SetValue("a")
		next, _ := m.updateEdit(tea.KeyMsg{Type: tea.KeyEnter})
		m = next.(model)
		if m.editErr == "" || m.editStep != 0 {
			t.Errorf("index %d: duplicate name accepted", index)
		}
	}

	// Keeping a feed's own name while editing is fine.
	m := testModel(Feed{"a", "u1"})
	m.startEdit(0)
	next, _ := m.updateEdit(tea.KeyMsg{Type: tea.KeyEnter})
	if next.(model).editStep != 1 {
		t.Error("editing a feed without renaming was rejected")
	}
}

func TestStaleFetchResultIsDropped(t *testing.T) {
	m := testModel(Feed{"a", "new-url"})
	next, _ := m.Update(feedFetchedMsg{feed: Feed{"a", "old-url"}, items: []NewsItem{{GUID: "x"}}})
	if _, ok := next.(model).states["a"]; ok {
		t.Error("result for an edited feed was applied")
	}
}

func TestTabCyclesScopesInOverviewOrder(t *testing.T) {
	m := testModel(Feed{"a", "u1"}, Feed{"b", "u2"})
	next, _ := m.updateOverview(tea.KeyMsg{Type: tea.KeyEnter}) // All feeds
	m = next.(model)
	press := func(k tea.KeyType) string {
		next, _ := m.updateItems(tea.KeyMsg{Type: k})
		m = next.(model)
		return m.scope
	}
	var got []string
	for range 4 {
		got = append(got, press(tea.KeyTab))
	}
	for range 2 {
		got = append(got, press(tea.KeyShiftTab))
	}
	if want := []string{"a", "b", "", "a", "", "b"}; !reflect.DeepEqual(got, want) || m.view != itemsView {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, f, _ := m.selectedFeed(); f.Name != "b" {
		t.Errorf("overview cursor out of sync with scope: %q", f.Name)
	}
}
