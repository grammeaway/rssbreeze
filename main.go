package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	cacheFileName     = "rssbreeze/seen.json"
	bookmarksFileName = "rssbreeze/bookmarks.json"
	feedsFileName     = "rssbreeze/feeds.json"
	defaultPageSize   = 50
)

var (
	version = "nightly"
	commit  = "unknown"
	date    = "unknown"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// FeedDoc decodes both RSS (<rss><channel><item>) and Atom (<feed><entry>); a document fills only one of the two.
type FeedDoc struct {
	Channel Channel     `xml:"channel"`
	Entries []AtomEntry `xml:"entry"`
}

type Channel struct {
	Items []Item `xml:"item"`
}

type Item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

type AtomEntry struct {
	Title     string `xml:"title"`
	ID        string `xml:"id"`
	Summary   string `xml:"summary"`
	Content   string `xml:"content"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
	Links     []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
}

func (e AtomEntry) item() Item {
	it := Item{Title: e.Title, GUID: e.ID, Description: e.Summary, PubDate: e.Published}
	if it.Description == "" {
		it.Description = e.Content
	}
	if it.PubDate == "" {
		it.PubDate = e.Updated
	}
	for _, l := range e.Links {
		// A link without rel is "alternate" per the Atom spec.
		if l.Rel == "" || l.Rel == "alternate" {
			it.Link = l.Href
			break
		}
	}
	if it.Link == "" && len(e.Links) > 0 {
		it.Link = e.Links[0].Href
	}
	return it
}

// Feed configuration
type Feed struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type FeedsConfig struct {
	Feeds    []Feed `json:"feeds"`
	PageSize int    `json:"page_size,omitempty"`
}

func (fc FeedsConfig) pageSize() int {
	if fc.PageSize > 0 {
		return fc.PageSize
	}
	return defaultPageSize
}

// Application state
type Config struct {
	LastSeen map[string]bool `json:"last_seen"`
}

// Bookmarks
type BookmarksFile struct {
	Bookmarks map[string]BookmarkEntry `json:"bookmarks"`
}

type BookmarkEntry struct {
	Title   string    `json:"title"`
	Link    string    `json:"link"`
	AddedAt time.Time `json:"added_at"`
}

type NewsItem struct {
	Title        string
	Link         string
	Description  string
	PubDate      time.Time
	GUID         string
	IsNew        bool
	IsBookmarked bool
	FeedName     string
}

func (i NewsItem) FilterValue() string { return i.Title }

// oneLine collapses all whitespace, newlines included, so text can't break the fixed-height layout.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

type itemDelegate struct{}

func (d itemDelegate) Height() int                             { return 3 }
func (d itemDelegate) Spacing() int                            { return 1 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(NewsItem)
	if !ok {
		return
	}

	isSelected := index == m.Index()
	width := m.Width()
	if isSelected {
		width -= 2 // selectedStyle padding
	}
	fit := func(s string) string { return ansi.Truncate(s, max(width, 0), "…") }

	titleStyle    := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	descStyle     := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	dateStyle     := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	feedStyle     := lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	newStyle      := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	bookmarkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)

	titleText := oneLine(i.Title)
	var title string
	prefix := ""
	if i.IsNew && i.IsBookmarked {
		prefix = newStyle.Render("● ") + bookmarkStyle.Render("★ ")
		title = titleStyle.Render(titleText)
	} else if i.IsNew {
		title = newStyle.Render("● " + titleText)
	} else if i.IsBookmarked {
		prefix = bookmarkStyle.Render("★ ")
		title = titleStyle.Render(titleText)
	} else {
		title = titleStyle.Render(titleText)
	}
	title = fit(prefix + title)

	desc := fit(descStyle.Render(oneLine(stripHTML(i.Description))))
	meta := fit(feedStyle.Render("["+oneLine(i.FeedName)+"]") + " " + dateStyle.Render(i.PubDate.Format("Jan 2, 2006")))

	if isSelected {
		selectedStyle := lipgloss.NewStyle().
			Background(lipgloss.Color("62")).
			Foreground(lipgloss.Color("15")).
			Padding(0, 1)
		fmt.Fprint(w, selectedStyle.Render(fmt.Sprintf("%s\n%s\n%s", title, desc, meta)))
	} else {
		fmt.Fprintf(w, "%s\n%s\n%s", title, desc, meta)
	}
}

// overviewItem is a row in the feed overview, rendered by list.DefaultDelegate.
type overviewItem struct{ title, desc string }

func (o overviewItem) Title() string       { return o.title }
func (o overviewItem) Description() string { return o.desc }
func (o overviewItem) FilterValue() string { return o.title }

type viewKind int

const (
	overviewView viewKind = iota
	itemsView
)

// feedState is what the current session knows about one feed, keyed by feed name.
type feedState struct {
	items   []NewsItem
	loaded  bool
	loading bool
	err     error
}

type model struct {
	view          viewKind
	overview      list.Model
	list          list.Model
	scope         string // feed name shown in itemsView; "" = all feeds
	states        map[string]feedState
	blendLimit    int // how many blended items are shown
	matchCount    int // items in scope matching the filters
	width, height int
	config        Config
	bookmarks     BookmarksFile
	feeds         FeedsConfig
	filterInput   textinput.Model
	filtering     bool
	filterDays    int
	showBookmarks bool
	showHelp      bool
	confirmDelete string
	editing       bool
	editIndex     int // -1 = adding a new feed
	editStep      int // 0 = name, 1 = URL
	editErr       string
	feedNameInput textinput.Model
	feedURLInput  textinput.Model
}

type feedFetchedMsg struct {
	feed  Feed
	items []NewsItem
	err   error
}

func initialModel() model {
	return newModel(loadFeeds(), loadConfig(), loadBookmarks())
}

func newModel(feeds FeedsConfig, config Config, bookmarks BookmarksFile) model {
	filterInput := textinput.New()
	filterInput.Placeholder = "Enter number of days (e.g., 7)"
	filterInput.Width = 20

	feedNameInput := textinput.New()
	feedNameInput.Placeholder = "e.g., Hacker News"
	feedNameInput.Width = 30

	feedURLInput := textinput.New()
	feedURLInput.Placeholder = "https://news.ycombinator.com/rss"
	feedURLInput.Width = 50

	titleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("15")).
		Background(lipgloss.Color("62")).
		Padding(0, 1).
		Bold(true)

	l := list.New(nil, itemDelegate{}, 0, 0)
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(false)
	l.Styles.Title = titleStyle

	o := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	o.Title = "rssbreeze"
	o.SetShowStatusBar(false)
	o.SetFilteringEnabled(false)
	o.Styles.Title = titleStyle

	m := model{
		overview:      o,
		list:          l,
		states:        map[string]feedState{},
		config:        config,
		bookmarks:     bookmarks,
		feeds:         feeds,
		filterInput:   filterInput,
		feedNameInput: feedNameInput,
		feedURLInput:  feedURLInput,
	}
	m.refreshOverview()
	return m
}

func (m model) Init() tea.Cmd { return nil }

func fetchFeed(feed Feed) ([]NewsItem, error) {
	resp, err := httpClient.Get(feed.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseFeed(body, feed.Name)
}

func parseFeed(body []byte, feedName string) ([]NewsItem, error) {
	var doc FeedDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	raw := doc.Channel.Items
	for _, e := range doc.Entries {
		raw = append(raw, e.item())
	}
	var items []NewsItem
	for _, item := range raw {
		guid := item.GUID
		if guid == "" {
			guid = item.Link
		}
		items = append(items, NewsItem{
			Title:       item.Title,
			Link:        item.Link,
			Description: item.Description,
			PubDate:     parseRSSDate(item.PubDate),
			GUID:        guid,
			FeedName:    feedName,
		})
	}
	return items, nil
}

// startFetch marks the feed as loading and returns the command that fetches it.
func (m *model) startFetch(f Feed) tea.Cmd {
	st := m.states[f.Name]
	st.loading = true
	m.states[f.Name] = st
	return func() tea.Msg {
		items, err := fetchFeed(f)
		return feedFetchedMsg{feed: f, items: items, err: err}
	}
}

// fetchScope fetches every feed in scope ("" = all). With force=false, feeds already loaded or loading are skipped.
func (m *model) fetchScope(scope string, force bool) tea.Cmd {
	var cmds []tea.Cmd
	for _, f := range m.feeds.Feeds {
		if scope != "" && f.Name != scope {
			continue
		}
		st, known := m.states[f.Name]
		if st.loading || (known && !force) {
			continue
		}
		cmds = append(cmds, m.startFetch(f))
	}
	m.refreshOverview()
	return tea.Batch(cmds...)
}

func (m *model) openScope(scope string) tea.Cmd {
	m.view = itemsView
	m.scope = scope
	m.blendLimit = m.feeds.pageSize()
	cmd := m.fetchScope(scope, false)
	m.applyFilters()
	m.list.ResetSelected()
	return cmd
}

func (m *model) hasFeed(f Feed) bool {
	for _, g := range m.feeds.Feeds {
		if g == f {
			return true
		}
	}
	return false
}

func (m *model) nameTaken(name string, except int) bool {
	for i, f := range m.feeds.Feeds {
		if i != except && f.Name == name {
			return true
		}
	}
	return false
}

// selectedFeed returns the feed under the overview cursor; ok is false on the "All feeds" row.
func (m *model) selectedFeed() (index int, f Feed, ok bool) {
	i := m.overview.Index() - 1
	if i < 0 || i >= len(m.feeds.Feeds) {
		return -1, Feed{}, false
	}
	return i, m.feeds.Feeds[i], true
}

func (m *model) startEdit(index int) {
	m.editing = true
	m.editIndex = index
	m.editStep = 0
	m.editErr = ""
	if index >= 0 {
		m.feedNameInput.SetValue(m.feeds.Feeds[index].Name)
		m.feedURLInput.SetValue(m.feeds.Feeds[index].URL)
		m.feedNameInput.CursorEnd()
		m.feedURLInput.CursorEnd()
	}
	m.feedNameInput.Focus()
}

func (m *model) stopEdit() {
	m.editing = false
	m.editStep = 0
	m.editErr = ""
	m.feedNameInput.SetValue("")
	m.feedURLInput.SetValue("")
	m.feedNameInput.Blur()
	m.feedURLInput.Blur()
}

func (m *model) saveEdit() {
	f := Feed{Name: strings.TrimSpace(m.feedNameInput.Value()), URL: strings.TrimSpace(m.feedURLInput.Value())}
	if m.editIndex < 0 {
		m.feeds.Feeds = append(m.feeds.Feeds, f)
	} else {
		// Any edit invalidates what we fetched; in-flight results for the old feed are dropped via hasFeed.
		delete(m.states, m.feeds.Feeds[m.editIndex].Name)
		m.feeds.Feeds[m.editIndex] = f
	}
	saveFeeds(m.feeds)
	m.stopEdit()
	m.refreshOverview()
}

func (m *model) deleteFeed(name string) {
	kept := make([]Feed, 0, len(m.feeds.Feeds))
	for _, f := range m.feeds.Feeds {
		if f.Name != name {
			kept = append(kept, f)
		}
	}
	m.feeds.Feeds = kept
	delete(m.states, name)
	saveFeeds(m.feeds)
	m.refreshOverview()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// The footer below the lists is always exactly one line.
		m.list.SetSize(msg.Width, msg.Height-1)
		m.overview.SetSize(msg.Width, msg.Height-1)
		return m, nil

	case feedFetchedMsg:
		if !m.hasFeed(msg.feed) {
			return m, nil // feed was edited or deleted meanwhile
		}
		st := m.states[msg.feed.Name]
		st.loading = false
		st.err = msg.err
		if msg.err == nil {
			st.items = msg.items
			st.loaded = true
		}
		m.states[msg.feed.Name] = st
		m.refreshOverview()
		if m.view == itemsView {
			m.applyFilters()
		}
		return m, nil

	case tea.KeyMsg:
		if m.editing {
			return m.updateEdit(msg)
		}

		if m.filtering {
			switch msg.String() {
			case "enter":
				m.filterDays = parseDays(m.filterInput.Value())
				m.filtering = false
				m.filterInput.Blur()
				m.applyFilters()
				return m, nil
			case "esc":
				m.filtering = false
				m.filterInput.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.filterInput, cmd = m.filterInput.Update(msg)
			return m, cmd
		}

		if m.confirmDelete != "" {
			if msg.String() == "y" {
				m.deleteFeed(m.confirmDelete)
			}
			m.confirmDelete = ""
			return m, nil
		}

		if m.showHelp {
			m.showHelp = false
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "q":
			m.saveConfig()
			return m, tea.Quit
		case "h":
			m.showHelp = true
			return m, nil
		}

		if m.view == overviewView {
			return m.updateOverview(msg)
		}
		return m.updateItems(msg)
	}

	var cmd tea.Cmd
	if m.view == overviewView {
		m.overview, cmd = m.overview.Update(msg)
	} else {
		m.list, cmd = m.list.Update(msg)
	}
	return m, cmd
}

func (m model) updateEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.stopEdit()
		return m, nil
	case "enter":
		if m.editStep == 0 {
			name := strings.TrimSpace(m.feedNameInput.Value())
			if name == "" {
				return m, nil
			}
			if m.nameTaken(name, m.editIndex) {
				m.editErr = fmt.Sprintf("A feed named %q already exists.", name)
				return m, nil
			}
			m.editErr = ""
			m.editStep = 1
			m.feedNameInput.Blur()
			m.feedURLInput.Focus()
			return m, nil
		}
		if strings.TrimSpace(m.feedURLInput.Value()) == "" {
			return m, nil
		}
		m.saveEdit()
		return m, nil
	}
	var cmd tea.Cmd
	if m.editStep == 0 {
		m.feedNameInput, cmd = m.feedNameInput.Update(msg)
	} else {
		m.feedURLInput, cmd = m.feedURLInput.Update(msg)
	}
	return m, cmd
}

func (m model) updateOverview(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.saveConfig()
		return m, tea.Quit
	case "enter":
		if len(m.feeds.Feeds) == 0 {
			return m, nil
		}
		_, f, ok := m.selectedFeed()
		if !ok {
			return m, m.openScope("")
		}
		return m, m.openScope(f.Name)
	case "a":
		m.startEdit(-1)
		return m, nil
	case "e":
		if i, _, ok := m.selectedFeed(); ok {
			m.startEdit(i)
		}
		return m, nil
	case "D":
		if _, f, ok := m.selectedFeed(); ok {
			m.confirmDelete = f.Name
		}
		return m, nil
	case "r":
		_, f, ok := m.selectedFeed()
		if !ok {
			return m, m.fetchScope("", true)
		}
		return m, m.fetchScope(f.Name, true)
	}
	var cmd tea.Cmd
	m.overview, cmd = m.overview.Update(msg)
	return m, cmd
}

func (m model) updateItems(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.view = overviewView
		m.refreshOverview()
		return m, nil

	case "enter":
		if item, ok := m.list.SelectedItem().(NewsItem); ok {
			m.markAsSeen(item.GUID)
			return m, openURL(item.Link)
		}
		return m, nil

	case "b":
		if item, ok := m.list.SelectedItem().(NewsItem); ok {
			m.toggleBookmark(item)
		}
		return m, nil

	case "B":
		m.showBookmarks = !m.showBookmarks
		m.applyFilters()
		return m, nil

	case "r":
		cmd := m.fetchScope(m.scope, true)
		m.applyFilters()
		return m, cmd

	case "f":
		m.filtering = true
		m.filterInput.Focus()
		return m, nil

	case "c":
		m.filterDays = 0
		m.showBookmarks = false
		m.applyFilters()
		return m, nil

	case "n":
		m.markAllAsSeen()
		m.applyFilters()
		return m, nil
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	// Blended pagination: reaching the last shown item reveals the next page.
	if m.scope == "" && m.blendLimit < m.matchCount && m.list.Index() >= len(m.list.Items())-1 {
		m.blendLimit += m.feeds.pageSize()
		m.applyFilters()
	}
	return m, cmd
}

func (m *model) toggleBookmark(item NewsItem) {
	if _, ok := m.bookmarks.Bookmarks[item.GUID]; ok {
		delete(m.bookmarks.Bookmarks, item.GUID)
	} else {
		m.bookmarks.Bookmarks[item.GUID] = BookmarkEntry{
			Title:   item.Title,
			Link:    item.Link,
			AddedAt: time.Now(),
		}
	}
	saveBookmarks(m.bookmarks)
	m.applyFilters()
}

// scopeItems returns the loaded items for the current scope, newest first, with seen/bookmark flags set.
func (m *model) scopeItems() []NewsItem {
	var all []NewsItem
	added := map[string]bool{} // guards against legacy duplicate feed names
	for _, f := range m.feeds.Feeds {
		if added[f.Name] || (m.scope != "" && f.Name != m.scope) {
			continue
		}
		added[f.Name] = true
		all = append(all, m.states[f.Name].items...)
	}
	for i := range all {
		_, seen := m.config.LastSeen[all[i].GUID]
		all[i].IsNew = !seen
		_, all[i].IsBookmarked = m.bookmarks.Bookmarks[all[i].GUID]
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].PubDate.After(all[j].PubDate)
	})
	return all
}

func (m *model) itemMatchesFilters(item NewsItem) bool {
	if m.showBookmarks && !item.IsBookmarked {
		return false
	}
	if m.filterDays > 0 && int(time.Since(item.PubDate).Hours()/24) > m.filterDays {
		return false
	}
	return true
}

func (m *model) applyFilters() {
	var filtered []list.Item
	for _, item := range m.scopeItems() {
		if m.itemMatchesFilters(item) {
			filtered = append(filtered, item)
		}
	}
	m.matchCount = len(filtered)
	if m.scope == "" && len(filtered) > m.blendLimit {
		filtered = filtered[:m.blendLimit]
	}
	m.list.SetItems(filtered)

	name := m.scope
	if name == "" {
		name = "All feeds"
	}
	title := "rssbreeze: " + oneLine(name)
	var parts []string
	if m.showBookmarks {
		parts = append(parts, "Bookmarks")
	}
	if m.filterDays > 0 {
		parts = append(parts, fmt.Sprintf("Last %d days", m.filterDays))
	}
	if len(parts) > 0 {
		title += " (" + strings.Join(parts, ", ") + ")"
	}
	if m.scope == "" && m.matchCount > len(filtered) {
		title += fmt.Sprintf(" · %d of %d", len(filtered), m.matchCount)
	}
	if m.scope != "" {
		if m.states[m.scope].loading {
			title += " · loading…"
		}
	} else {
		loading := 0
		for _, st := range m.states {
			if st.loading {
				loading++
			}
		}
		if loading > 0 {
			title += fmt.Sprintf(" · loading %d feed(s)…", loading)
		}
	}
	m.list.Title = title
}

func (m *model) feedStatus(name string) string {
	st, ok := m.states[name]
	switch {
	case !ok:
		return "not loaded"
	case st.loading:
		return "loading…"
	case st.err != nil:
		return "error: " + st.err.Error()
	}
	return fmt.Sprintf("%d items, %d new", len(st.items), m.countNew(st.items))
}

func (m *model) countNew(items []NewsItem) int {
	n := 0
	for _, item := range items {
		if _, seen := m.config.LastSeen[item.GUID]; !seen {
			n++
		}
	}
	return n
}

func (m *model) refreshOverview() {
	loaded, newCount := 0, 0
	rows := []list.Item{nil}
	for _, f := range m.feeds.Feeds {
		if st := m.states[f.Name]; st.loaded {
			loaded++
			newCount += m.countNew(st.items)
		}
		rows = append(rows, overviewItem{
			title: oneLine(f.Name),
			desc:  oneLine(m.feedStatus(f.Name) + " · " + f.URL),
		})
	}
	rows[0] = overviewItem{
		title: "All feeds",
		desc:  fmt.Sprintf("%d of %d feeds loaded, %d new", loaded, len(m.feeds.Feeds), newCount),
	}
	m.overview.SetItems(rows)
}

func (m *model) markAsSeen(guid string) {
	m.config.LastSeen[guid] = true
	m.applyFilters()
}

func (m *model) markAllAsSeen() {
	for _, item := range m.scopeItems() {
		if m.itemMatchesFilters(item) {
			m.config.LastSeen[item.GUID] = true
		}
	}
	m.saveConfig()
}

func parseDays(input string) int {
	days, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || days < 0 {
		return 0
	}
	return days
}

const helpText = `Controls

Overview:
  ↑/↓ or j/k  - Navigate feeds
  Enter       - Open selected feed (or All feeds)
  a           - Add a new RSS feed
  e           - Edit selected feed's name and URL
  D           - Delete selected feed
  r           - Refresh selected feed (or all feeds)
  q / esc     - Quit

Feed items:
  ↑/↓ or j/k  - Navigate items (All feeds loads more at the bottom)
  Enter       - Open selected item in browser
  b           - Toggle bookmark on selected item
  B           - Toggle bookmarks-only filter
  f           - Filter by date (days)
  c           - Clear all filters
  n           - Mark visible items as seen
  r           - Refresh the feed(s) in view
  esc         - Back to the overview
  q           - Quit

● Green dots indicate unread items.
★ Yellow stars indicate bookmarked items.

Press any key to close this help.`

// View renders exactly one body (list, help or modal) plus a one-line footer, so the
// layout never outgrows the terminal and pushes the list title off-screen.
func (m model) View() string {
	footer := "Press 'h' for help"
	if m.confirmDelete != "" {
		footer = fmt.Sprintf("Delete feed %q? (y/n)", oneLine(m.confirmDelete))
	}

	modal := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1)
	var body string
	switch {
	case m.showHelp:
		body = helpText
	case m.editing:
		heading := "Add RSS feed"
		if m.editIndex >= 0 {
			heading = "Edit RSS feed"
		}
		var content string
		if m.editStep == 0 {
			content = fmt.Sprintf("%s\n\nFeed name:\n%s\n\nPress Enter to continue, Esc to cancel", heading, m.feedNameInput.View())
		} else {
			content = fmt.Sprintf("%s\n\nFeed name: %s\nFeed URL:\n%s\n\nPress Enter to save, Esc to cancel", heading, m.feedNameInput.Value(), m.feedURLInput.View())
		}
		if m.editErr != "" {
			content += "\n\n" + m.editErr
		}
		body = modal.Render(content)
	case m.filtering:
		body = modal.Render(fmt.Sprintf("Filter by days:\n%s\n\nPress Enter to apply, Esc to cancel", m.filterInput.View()))
	case m.view == overviewView && len(m.feeds.Feeds) == 0:
		body = "No feeds configured.\n\nPress 'a' to add your first RSS feed.\nPress 'q' to quit."
	case m.view == overviewView:
		body = m.overview.View()
	default:
		body = m.list.View()
	}

	return lipgloss.JoinVertical(lipgloss.Left, body, footer)
}

// --- Feed persistence ---

func loadFeeds() FeedsConfig {
	fc, _ := readFeeds()
	return fc
}

// readFeeds is loadFeeds with errors; a missing file is not an error.
func readFeeds() (FeedsConfig, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return FeedsConfig{}, err
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, feedsFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return FeedsConfig{}, nil
	}
	if err != nil {
		return FeedsConfig{}, err
	}
	var fc FeedsConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return FeedsConfig{}, err
	}
	return fc, nil
}

func saveFeeds(fc FeedsConfig) error {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	path := filepath.Join(cacheDir, feedsFileName)
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return err
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// mergeFeeds adds incoming feeds to local, skipping known URLs and names. Idempotent; local settings are kept.
func mergeFeeds(local FeedsConfig, incoming []Feed) (merged FeedsConfig, added int, warnings []string) {
	merged = local
	merged.Feeds = append([]Feed(nil), local.Feeds...)
	for _, f := range incoming {
		f.Name, f.URL = strings.TrimSpace(f.Name), strings.TrimSpace(f.URL)
		if f.Name == "" || f.URL == "" {
			warnings = append(warnings, fmt.Sprintf("skipped feed with empty name or URL: %q %q", f.Name, f.URL))
			continue
		}
		skip := false
		for _, g := range merged.Feeds {
			if g.URL == f.URL {
				skip = true
				break
			}
			if g.Name == f.Name {
				warnings = append(warnings, fmt.Sprintf("skipped %q: name already used for %s", f.Name, g.URL))
				skip = true
				break
			}
		}
		if !skip {
			merged.Feeds = append(merged.Feeds, f)
			added++
		}
	}
	return merged, added, warnings
}

func runExport(args []string) error {
	fc, err := readFeeds()
	if err != nil {
		return err
	}
	if fc.Feeds == nil {
		fc.Feeds = []Feed{}
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	if len(args) == 0 {
		_, err = fmt.Println(string(data))
		return err
	}
	return os.WriteFile(args[0], data, 0644)
}

func runImport(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rssbreeze import <file|->")
	}
	var data []byte
	var err error
	if args[0] == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(args[0])
	}
	if err != nil {
		return err
	}
	var incoming FeedsConfig
	if err := json.Unmarshal(data, &incoming); err != nil {
		return fmt.Errorf("invalid feeds file: %w", err)
	}
	// Strict read: never overwrite a local feeds.json we failed to parse.
	local, err := readFeeds()
	if err != nil {
		return fmt.Errorf("reading local feeds: %w", err)
	}
	merged, added, warnings := mergeFeeds(local, incoming.Feeds)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if added > 0 {
		if err := saveFeeds(merged); err != nil {
			return err
		}
	}
	fmt.Printf("added %d, skipped %d\n", added, len(incoming.Feeds)-added)
	return nil
}

// --- Bookmark persistence ---

func loadBookmarks() BookmarksFile {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return BookmarksFile{Bookmarks: make(map[string]BookmarkEntry)}
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, bookmarksFileName))
	if err != nil {
		return BookmarksFile{Bookmarks: make(map[string]BookmarkEntry)}
	}
	var bf BookmarksFile
	if err := json.Unmarshal(data, &bf); err != nil {
		return BookmarksFile{Bookmarks: make(map[string]BookmarkEntry)}
	}
	if bf.Bookmarks == nil {
		bf.Bookmarks = make(map[string]BookmarkEntry)
	}
	return bf
}

func saveBookmarks(bf BookmarksFile) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	path := filepath.Join(cacheDir, bookmarksFileName)
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return
	}
	data, err := json.MarshalIndent(bf, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing bookmarks: %v\n", err)
	}
}

// --- Config / seen-items persistence ---

func loadConfig() Config {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return Config{LastSeen: make(map[string]bool)}
	}
	// Ensure cache dir exists
	os.MkdirAll(filepath.Join(cacheDir, "rssbreeze"), os.ModePerm)

	data, err := os.ReadFile(filepath.Join(cacheDir, cacheFileName))
	if err != nil {
		return Config{LastSeen: make(map[string]bool)}
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{LastSeen: make(map[string]bool)}
	}
	if config.LastSeen == nil {
		config.LastSeen = make(map[string]bool)
	}
	return config
}

func (m *model) saveConfig() {
	m.cleanupConfig()
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	data, err := json.Marshal(m.config)
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(cacheDir, cacheFileName), data, 0644)
}

// cleanupConfig prunes seen GUIDs that no feed lists anymore. It only runs once every
// configured feed has loaded successfully this session; otherwise the seen state of
// feeds that weren't opened would be wiped.
func (m *model) cleanupConfig() {
	if m.config.LastSeen == nil || len(m.feeds.Feeds) == 0 {
		return
	}
	currentGUIDs := make(map[string]bool)
	for _, f := range m.feeds.Feeds {
		st := m.states[f.Name]
		if !st.loaded || st.err != nil {
			return
		}
		for _, item := range st.items {
			currentGUIDs[item.GUID] = true
		}
	}
	newLastSeen := make(map[string]bool)
	for guid, seen := range m.config.LastSeen {
		if currentGUIDs[guid] {
			newLastSeen[guid] = seen
		}
	}
	m.config.LastSeen = newLastSeen
}

// sanitizeURL fixes malformed URLs sometimes found in RSS feeds, such as a
// missing '/' between the domain and the path (e.g., "example.compath" →
// "example.com/path"). Uses net/url parsing to detect when path content has
// been absorbed into the host component.
func sanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	hostname := u.Hostname()
	port := ""
	if p := u.Port(); p != "" {
		port = ":" + p
	}
	for _, tld := range []string{".com", ".org", ".net", ".io", ".dev", ".gov", ".edu"} {
		idx := strings.Index(hostname, tld)
		if idx == -1 {
			continue
		}
		after := idx + len(tld)
		// Only fix if the character after the TLD is not '.', ':', or end of string
		// (a dot would indicate a subdomain like .com.au; a colon would be a port)
		if after < len(hostname) && hostname[after] != '.' && hostname[after] != ':' {
			absorbed := hostname[after:]
			u.Host = hostname[:after] + port
			u.Path = "/" + absorbed + u.Path
			return u.String()
		}
	}
	return rawURL
}

func openURL(rawURL string) tea.Cmd {
	return func() tea.Msg {
		rawURL = sanitizeURL(rawURL)
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "linux":
			cmd = exec.Command("xdg-open", rawURL)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
		case "darwin":
			cmd = exec.Command("open", rawURL)
		default:
			return nil
		}
		cmd.Start()
		return nil
	}
}

func stripHTML(s string) string {
	result := strings.Builder{}
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
		} else if r == '>' {
			inTag = false
		} else if !inTag {
			result.WriteRune(r)
		}
	}
	return strings.TrimSpace(result.String())
}

func parseRSSDate(dateStr string) time.Time {
	formats := []string{
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 02 Jan 2006 15:04:05 GMT",
		time.RFC3339, // also accepts fractional seconds, common in Atom
		"2006-01-02 15:04:05",
		"Jan 02, 2006 15:04:05",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, dateStr); err == nil {
			return t
		}
	}
	return time.Now()
}

func main() {
	if len(os.Args) > 1 {
		var run func([]string) error
		switch os.Args[1] {
		case "version":
			fmt.Printf("rssbreeze version: %s\ncommit: %s\nbuilt at: %s\n", version, commit, date)
			return
		case "export":
			run = runExport
		case "import":
			run = runImport
		}
		if run != nil {
			if err := run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}
			return
		}
	}

	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v", err)
		os.Exit(1)
	}
}
