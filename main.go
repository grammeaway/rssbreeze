package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
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
)

const (
	cacheFileName     = "rssbreeze/seen.json"
	bookmarksFileName = "rssbreeze/bookmarks.json"
	feedsFileName     = "rssbreeze/feeds.json"
)

var (
	version = "nightly"
	commit  = "unknown"
	date    = "unknown"
)

// RSS structures
type RSS struct {
	Channel Channel `xml:"channel"`
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

// Feed configuration
type Feed struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type FeedsConfig struct {
	Feeds []Feed `json:"feeds"`
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

	titleStyle    := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	descStyle     := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	dateStyle     := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	feedStyle     := lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	newStyle      := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	bookmarkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)

	var title string
	prefix := ""
	if i.IsNew && i.IsBookmarked {
		prefix = newStyle.Render("● ") + bookmarkStyle.Render("★ ")
		title = titleStyle.Render(i.Title)
	} else if i.IsNew {
		title = newStyle.Render("● " + i.Title)
	} else if i.IsBookmarked {
		prefix = bookmarkStyle.Render("★ ")
		title = titleStyle.Render(i.Title)
	} else {
		title = titleStyle.Render(i.Title)
	}
	if prefix != "" {
		title = prefix + title
	}

	desc := strings.TrimSpace(stripHTML(i.Description))
	if len(desc) > 80 {
		desc = desc[:77] + "..."
	}
	desc = descStyle.Render(desc)

	meta := feedStyle.Render("["+i.FeedName+"]") + " " + dateStyle.Render(i.PubDate.Format("Jan 2, 2006"))

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

type model struct {
	list          list.Model
	items         []NewsItem
	config        Config
	bookmarks     BookmarksFile
	feeds         FeedsConfig
	loading       bool
	err           error
	filterInput   textinput.Model
	filtering     bool
	filterDays    int
	filterFeed    string
	showBookmarks bool
	showHelp      bool
	addingFeed    bool
	addFeedStep   int // 0 = name, 1 = URL
	feedNameInput textinput.Model
	feedURLInput  textinput.Model
}

type fetchedMsg []NewsItem
type errMsg error

func initialModel() model {
	feeds     := loadFeeds()
	config    := loadConfig()
	bookmarks := loadBookmarks()

	filterInput := textinput.New()
	filterInput.Placeholder = "Enter number of days (e.g., 7)"
	filterInput.Width = 20

	feedNameInput := textinput.New()
	feedNameInput.Placeholder = "e.g., Hacker News"
	feedNameInput.Width = 30

	feedURLInput := textinput.New()
	feedURLInput.Placeholder = "https://news.ycombinator.com/rss"
	feedURLInput.Width = 50

	items := []list.Item{}
	l := list.New(items, itemDelegate{}, 0, 0)
	l.Title = "rssbreeze"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(false)
	l.Styles.Title = lipgloss.NewStyle().
		Foreground(lipgloss.Color("15")).
		Background(lipgloss.Color("62")).
		Padding(0, 1).
		Bold(true)

	return model{
		list:          l,
		config:        config,
		bookmarks:     bookmarks,
		feeds:         feeds,
		loading:       len(feeds.Feeds) > 0,
		filterInput:   filterInput,
		feedNameInput: feedNameInput,
		feedURLInput:  feedURLInput,
	}
}

func (m model) Init() tea.Cmd {
	if len(m.feeds.Feeds) == 0 {
		return nil
	}
	return fetchAllFeeds(m.feeds.Feeds)
}

func fetchAllFeeds(feeds []Feed) tea.Cmd {
	return func() tea.Msg {
		type result struct {
			items []NewsItem
			err   error
		}
		ch := make(chan result, len(feeds))
		for _, feed := range feeds {
			go func(f Feed) {
				items, err := fetchFeed(f)
				ch <- result{items, err}
			}(feed)
		}
		var allItems []NewsItem
		for range feeds {
			r := <-ch
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "Error fetching feed: %v\n", r.err)
				continue
			}
			allItems = append(allItems, r.items...)
		}
		sort.Slice(allItems, func(i, j int) bool {
			return allItems[i].PubDate.After(allItems[j].PubDate)
		})
		return fetchedMsg(allItems)
	}
}

func fetchFeed(feed Feed) ([]NewsItem, error) {
	resp, err := http.Get(feed.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rss RSS
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, err
	}

	var items []NewsItem
	for _, item := range rss.Channel.Items {
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
			FeedName:    feed.Name,
		})
	}
	return items, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetWidth(msg.Width)
		m.list.SetHeight(msg.Height - 3)
		return m, nil

	case fetchedMsg:
		m.loading = false
		m.err = nil
		m.items = []NewsItem(msg)
		for i := range m.items {
			_, seen := m.config.LastSeen[m.items[i].GUID]
			m.items[i].IsNew = !seen
			_, bookmarked := m.bookmarks.Bookmarks[m.items[i].GUID]
			m.items[i].IsBookmarked = bookmarked
		}
		m.applyFilters()
		return m, nil

	case errMsg:
		m.loading = false
		m.err = msg
		return m, nil

	case tea.KeyMsg:
		// Add-feed input mode
		if m.addingFeed {
			switch msg.String() {
			case "esc":
				m.addingFeed = false
				m.addFeedStep = 0
				m.feedNameInput.SetValue("")
				m.feedURLInput.SetValue("")
				m.feedNameInput.Blur()
				m.feedURLInput.Blur()
				return m, nil
			case "enter":
				if m.addFeedStep == 0 {
					if strings.TrimSpace(m.feedNameInput.Value()) == "" {
						return m, nil
					}
					m.addFeedStep = 1
					m.feedNameInput.Blur()
					m.feedURLInput.Focus()
					return m, nil
				}
				rawURL := strings.TrimSpace(m.feedURLInput.Value())
				if rawURL == "" {
					return m, nil
				}
				name := strings.TrimSpace(m.feedNameInput.Value())
				m.feeds.Feeds = append(m.feeds.Feeds, Feed{Name: name, URL: rawURL})
				saveFeeds(m.feeds)
				m.addingFeed = false
				m.addFeedStep = 0
				m.feedNameInput.SetValue("")
				m.feedURLInput.SetValue("")
				m.feedNameInput.Blur()
				m.feedURLInput.Blur()
				m.loading = true
				return m, fetchAllFeeds(m.feeds.Feeds)
			}
			var cmd tea.Cmd
			if m.addFeedStep == 0 {
				m.feedNameInput, cmd = m.feedNameInput.Update(msg)
			} else {
				m.feedURLInput, cmd = m.feedURLInput.Update(msg)
			}
			return m, cmd
		}

		// Date-filter input mode
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

		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.saveConfig()
			return m, tea.Quit

		case "enter":
			if len(m.list.Items()) > 0 {
				selected := m.list.SelectedItem().(NewsItem)
				m.markAsSeen(selected.GUID)
				return m, openURL(selected.Link)
			}

		case "b":
			if len(m.list.Items()) > 0 {
				selected := m.list.SelectedItem().(NewsItem)
				m.toggleBookmark(selected.GUID)
			}
			return m, nil

		case "B":
			m.showBookmarks = !m.showBookmarks
			m.applyFilters()
			return m, nil

		case "r":
			if len(m.feeds.Feeds) > 0 {
				m.loading = true
				return m, fetchAllFeeds(m.feeds.Feeds)
			}
			return m, nil

		case "f":
			m.filtering = true
			m.filterInput.Focus()
			return m, nil

		case "F":
			// Cycle through feed filter: all → feed1 → feed2 → ... → all
			if len(m.feeds.Feeds) == 0 {
				return m, nil
			}
			if m.filterFeed == "" {
				m.filterFeed = m.feeds.Feeds[0].Name
			} else {
				cycled := false
				for i, f := range m.feeds.Feeds {
					if f.Name == m.filterFeed {
						if i+1 < len(m.feeds.Feeds) {
							m.filterFeed = m.feeds.Feeds[i+1].Name
						} else {
							m.filterFeed = ""
						}
						cycled = true
						break
					}
				}
				if !cycled {
					m.filterFeed = ""
				}
			}
			m.applyFilters()
			return m, nil

		case "a":
			m.addingFeed = true
			m.addFeedStep = 0
			m.feedNameInput.Focus()
			return m, nil

		case "D":
			// Delete the feed currently active in the feed filter
			if m.filterFeed == "" {
				return m, nil
			}
			newFeeds := make([]Feed, 0, len(m.feeds.Feeds))
			for _, f := range m.feeds.Feeds {
				if f.Name != m.filterFeed {
					newFeeds = append(newFeeds, f)
				}
			}
			m.feeds.Feeds = newFeeds
			saveFeeds(m.feeds)
			m.filterFeed = ""
			if len(m.feeds.Feeds) > 0 {
				m.loading = true
				return m, fetchAllFeeds(m.feeds.Feeds)
			}
			m.items = nil
			m.applyFilters()
			return m, nil

		case "c":
			m.filterDays = 0
			m.showBookmarks = false
			m.filterFeed = ""
			m.applyFilters()
			return m, nil

		case "n":
			m.markAllAsSeen()
			m.applyFilters()
			return m, nil

		case "h":
			m.showHelp = !m.showHelp
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *model) toggleBookmark(guid string) {
	var target *NewsItem
	for i := range m.items {
		if m.items[i].GUID == guid {
			target = &m.items[i]
			break
		}
	}
	if target == nil {
		return
	}
	if target.IsBookmarked {
		delete(m.bookmarks.Bookmarks, guid)
		target.IsBookmarked = false
	} else {
		if m.bookmarks.Bookmarks == nil {
			m.bookmarks.Bookmarks = make(map[string]BookmarkEntry)
		}
		m.bookmarks.Bookmarks[guid] = BookmarkEntry{
			Title:   target.Title,
			Link:    target.Link,
			AddedAt: time.Now(),
		}
		target.IsBookmarked = true
	}
	saveBookmarks(m.bookmarks)
	m.applyFilters()
}

func (m *model) applyFilters() {
	var filtered []list.Item
	for _, item := range m.items {
		if m.showBookmarks && !item.IsBookmarked {
			continue
		}
		if m.filterDays > 0 && int(time.Since(item.PubDate).Hours()/24) > m.filterDays {
			continue
		}
		if m.filterFeed != "" && item.FeedName != m.filterFeed {
			continue
		}
		filtered = append(filtered, item)
	}
	m.list.SetItems(filtered)

	title := "rssbreeze"
	var parts []string
	if m.filterFeed != "" {
		parts = append(parts, m.filterFeed)
	}
	if m.showBookmarks {
		parts = append(parts, "Bookmarks")
	}
	if m.filterDays > 0 {
		parts = append(parts, fmt.Sprintf("Last %d days", m.filterDays))
	}
	if len(parts) > 0 {
		title += " (" + strings.Join(parts, ", ") + ")"
	}
	m.list.Title = title
}

func (m *model) markAsSeen(guid string) {
	if m.config.LastSeen == nil {
		m.config.LastSeen = make(map[string]bool)
	}
	m.config.LastSeen[guid] = true
	for i := range m.items {
		if m.items[i].GUID == guid {
			m.items[i].IsNew = false
			break
		}
	}
	m.applyFilters()
}

func (m *model) markAllAsSeen() {
	if m.config.LastSeen == nil {
		m.config.LastSeen = make(map[string]bool)
	}
	for i := range m.items {
		m.config.LastSeen[m.items[i].GUID] = true
		m.items[i].IsNew = false
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

func (m model) View() string {
	if len(m.feeds.Feeds) == 0 && !m.addingFeed {
		return "No feeds configured.\n\nPress 'a' to add your first RSS feed.\nPress 'q' to quit."
	}

	if m.loading {
		return fmt.Sprintf("Fetching news from %d feed(s)...\n\nPress 'q' to quit", len(m.feeds.Feeds))
	}

	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress 'r' to retry or 'q' to quit", m.err)
	}

	var view string
	if len(m.feeds.Feeds) == 0 {
		// addingFeed must be true
		view = "No feeds configured yet."
	} else {
		view = m.list.View()
	}

	if m.filtering {
		filterView := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(1).
			Render(fmt.Sprintf("Filter by days:\n%s\n\nPress Enter to apply, Esc to cancel", m.filterInput.View()))
		view = lipgloss.JoinVertical(lipgloss.Left, view, filterView)
	}

	if m.addingFeed {
		var content string
		if m.addFeedStep == 0 {
			content = fmt.Sprintf("Add RSS feed\n\nFeed name:\n%s\n\nPress Enter to continue, Esc to cancel", m.feedNameInput.View())
		} else {
			content = fmt.Sprintf("Add RSS feed\n\nFeed name: %s\nFeed URL:\n%s\n\nPress Enter to add, Esc to cancel", m.feedNameInput.Value(), m.feedURLInput.View())
		}
		addView := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(1).
			Render(content)
		view = lipgloss.JoinVertical(lipgloss.Left, view, addView)
	}

	statusLine := "Press 'h' for help"
	if m.showHelp {
		var feedNames []string
		for _, f := range m.feeds.Feeds {
			feedNames = append(feedNames, f.Name)
		}
		feedList := ""
		if len(feedNames) > 0 {
			feedList = "\nConfigured feeds: " + strings.Join(feedNames, ", ")
		}
		statusLine = `Controls:
  ↑/↓ or j/k  - Navigate items
  Enter       - Open selected item in browser
  b           - Toggle bookmark on selected item
  B           - Toggle bookmarks-only filter
  r           - Refresh all feeds
  f           - Filter by date (days)
  F           - Cycle through feed filter
  a           - Add a new RSS feed
  D           - Delete the active feed filter's feed
  c           - Clear all filters
  n           - Mark all as seen
  h           - Toggle this help
  q           - Quit

● Green dots indicate unread items.
★ Yellow stars indicate bookmarked items.` + feedList
	}

	return lipgloss.JoinVertical(lipgloss.Left, view, statusLine)
}

// --- Feed persistence ---

func loadFeeds() FeedsConfig {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return FeedsConfig{}
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, feedsFileName))
	if err != nil {
		return FeedsConfig{}
	}
	var fc FeedsConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return FeedsConfig{}
	}
	return fc
}

func saveFeeds(fc FeedsConfig) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	path := filepath.Join(cacheDir, feedsFileName)
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0644)
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

func (m *model) cleanupConfig() {
	if m.config.LastSeen == nil {
		return
	}
	currentGUIDs := make(map[string]bool, len(m.items))
	for _, item := range m.items {
		currentGUIDs[item.GUID] = true
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
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		"Jan 02, 2006 15:04:05",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, dateStr); err == nil {
			return t
		}
	}
	fmt.Fprintf(os.Stderr, "Warning: could not parse date %q, using current time\n", dateStr)
	return time.Now()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("rssbreeze version: %s\ncommit: %s\nbuilt at: %s\n", version, commit, date)
		return
	}

	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v", err)
		os.Exit(1)
	}
}
