package main

// Feeds. An account can follow RSS and Atom feeds. The list of an account
// is one plain file, feeds.txt, with one feed on each line:
//
//	URL<tab>name
//
// The command `sbm-sync feed` (feedjob.go) fetches the feeds with
// sfeed_update into one sfeed(5) file for each feed, looks through the
// bookmarks of the account for feeds with sfeed_web when the account asked
// for it, and sends a daily digest of the new items by email. The page /feed
// shows the items, the feeds and the settings. GET /api/feed gives the items
// in the sfeed format, for sfeed_plain and the other sfeed tools.
//
// The files of an account are under feeds/<account ID>/:
//
//	feeds.txt    the feeds, written by the web pages
//	sfeedrc      the sfeed_update settings, made from feeds.txt by the job
//	items/<file> the items of one feed, written by sfeed_update
//	explore      the feeds found on the bookmarked pages, written by the job
//	mailed       the keys of the items that went out in a digest
//	digest       the time of the last digest

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxFeeds    = 200  // feeds that an account can follow
	maxFeedURL  = 2048 // bytes in the URL of a feed
	maxFeedName = 100  // characters in the name of a feed
	maxFileName = 40   // characters in the file name of a feed, before a number
	itemsOnPage = 100
)

var (
	errFeedURL   = errors.New("the URL must start with http:// or https:// and have no spaces or quotes")
	errFeedLong  = errors.New("the URL is longer than 2048 characters")
	errFeedKnown = errors.New("you follow this feed already")
	errFeedMany  = errors.New("you follow 200 feeds already, the most that this server keeps")
	notFile      = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// feedSettings are the feed choices of an account.
type feedSettings struct {
	NewTab  bool `json:"newtab,omitempty"`  // the feed page opens links in a new tab
	Mail    bool `json:"mail,omitempty"`    // an email with the new items, once a day
	Explore bool `json:"explore,omitempty"` // the job looks through the bookmarks for feeds
}

// Feed is one line of feeds.txt. File is the name of the item file of the
// feed under items/, and the name that sfeed_update knows the feed by.
type Feed struct {
	URL, Name, File string
}

// feedURL checks the URL of a feed. It takes only http and https addresses
// of printable ASCII without quotes, so that the URL can go into the
// sfeedrc, a shell script, between single quotes.
func feedURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", errNoURL
	}
	if len(u) > maxFeedURL {
		return "", errFeedLong
	}
	for i := 0; i < len(u); i++ {
		if c := u[i]; c <= ' ' || c > '~' || c == '\'' {
			return "", errFeedURL
		}
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https") {
		return "", errFeedURL
	}
	return u, nil
}

// feedName gives the name of a feed for feeds.txt: one line of at most
// maxFeedName characters. Without a name, the host of the URL is the name.
func feedName(name, u string) string {
	n := strings.Join(strings.Fields(name), " ")
	if n == "" {
		n = Bookmark{URL: u}.Host()
	}
	if r := []rune(n); len(r) > maxFeedName {
		n = string(r[:maxFeedName])
	}
	return n
}

// fileName gives the base of the file name of a feed: the letters, digits,
// dots, dashes and underscores of its name, or "feed".
func fileName(name string) string {
	f := strings.Trim(notFile.ReplaceAllString(name, "-"), "-.")
	if len(f) > maxFileName {
		f = strings.Trim(f[:maxFileName], "-.")
	}
	if f == "" {
		return "feed"
	}
	return f
}

// parseFeeds gives the feeds of feeds.txt, in the order of the file. Each
// feed gets a file name that no other feed of the file has.
func parseFeeds(text string) []Feed {
	var out []Feed
	used := map[string]bool{}
	for _, l := range lines(text) {
		u, name, _ := strings.Cut(l, "\t")
		if u == "" {
			continue
		}
		f := Feed{URL: u, Name: name, File: fileName(name)}
		for i := 2; used[f.File]; i++ {
			f.File = fileName(name) + "-" + strconv.Itoa(i)
		}
		used[f.File] = true
		out = append(out, f)
	}
	return out
}

// Item is one line of an sfeed(5) file.
type Item struct {
	Time   time.Time // zero when the feed gives none
	Title  string
	Link   string // the address, or "" when it is not http or https
	Author string
	Feed   string // the name of the feed
	File   string // the item file of the feed
	Key    string // "file<tab>id" names the item in mailed; the link or the title stand in for a missing id
	Raw    string // the line as sfeed wrote it
}

// unescape undoes the escapes of the sfeed format: \t, \n and \\.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseItems gives the items of the sfeed file of a feed, in the order of
// the file.
func parseItems(text string, f Feed) []Item {
	var out []Item
	for _, l := range lines(text) {
		c := strings.Split(l, "\t")
		if len(c) < 3 {
			continue
		}
		for len(c) < 9 {
			c = append(c, "")
		}
		it := Item{Title: unescape(c[1]), Author: unescape(c[6]), Feed: f.Name, File: f.File, Raw: l}
		if t, err := strconv.ParseInt(c[0], 10, 64); err == nil {
			it.Time = time.Unix(t, 0).UTC()
		}
		link := unescape(c[2])
		if link != "" {
			it.Link = Bookmark{URL: link}.Link()
		}
		id := c[5]
		if id == "" {
			id = c[2]
		}
		if id == "" {
			id = c[1]
		}
		it.Key = f.File + "\t" + id
		if it.Title == "" {
			it.Title = link
		}
		if it.Title == "" {
			it.Title = "(no title)"
		}
		out = append(out, it)
	}
	return out
}

// ---- the files of an account ----

func (s *Store) feedDir(id string) string {
	return filepath.Join(s.dir, "feeds", id)
}

// readFeeds gives feeds.txt of the account. A missing file is empty.
func (s *Store) readFeeds(a *Account) (string, error) {
	m := s.lock(a.ID)
	m.Lock()
	defer m.Unlock()
	b, err := os.ReadFile(filepath.Join(s.feedDir(a.ID), "feeds.txt"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// changeFeeds gives feeds.txt to f and writes what f gives back. No other
// change to the file comes between the two.
func (s *Store) changeFeeds(a *Account, f func(text string) (string, error)) error {
	m := s.lock(a.ID)
	m.Lock()
	defer m.Unlock()
	dir := s.feedDir(a.ID)
	b, err := os.ReadFile(filepath.Join(dir, "feeds.txt"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	text, err := f(string(b))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "feeds.txt"), []byte(text))
}

func (s *Store) setFeed(a *Account, f feedSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.Feed = f
	return s.save()
}

// all gives every account, oldest first.
func (s *Store) all() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y *Account) int { return x.Created.Compare(y.Created) })
	return out
}

// feedItems gives the items of the feeds of the account, newest first.
// Items without a time come last.
func (s *server) feedItems(a *Account, feeds []Feed) []Item {
	var all []Item
	for _, f := range feeds {
		b, err := os.ReadFile(filepath.Join(s.store.feedDir(a.ID), "items", f.File))
		if err != nil {
			continue // not fetched yet
		}
		all = append(all, parseItems(string(b), f)...)
	}
	slices.SortStableFunc(all, func(x, y Item) int { return y.Time.Compare(x.Time) })
	return all
}

// suggestion is a feed that the job found on a bookmarked page.
type suggestion struct {
	URL, Page, Name string
}

// suggestions gives the feeds of the explore file that the account does not
// follow, the number of bookmarked pages that the job looked at, and the
// number of bookmarks with a web address.
func (s *server) suggestions(a *Account, feeds []Feed) ([]suggestion, int, int, error) {
	text, _, err := s.store.read(a, "")
	if err != nil {
		return nil, 0, 0, err
	}
	names := map[string]string{}
	total := 0
	for _, b := range parse(text) {
		if l := b.Link(); l != "" {
			names[l] = b.Desc
			total++
		}
	}
	b, err := os.ReadFile(filepath.Join(s.store.feedDir(a.ID), "explore"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, 0, err
	}
	known := map[string]bool{}
	for _, f := range feeds {
		known[norm(f.URL)] = true
	}
	pages := map[string]bool{}
	var out []suggestion
	for _, l := range lines(string(b)) {
		f := strings.Split(l, "\t")
		if len(f) < 2 {
			continue
		}
		if _, ok := names[f[0]]; ok {
			pages[f[0]] = true // a page that is a bookmark still
		}
		if f[1] == "-" || known[norm(f[1])] {
			continue
		}
		known[norm(f[1])] = true
		out = append(out, suggestion{URL: f[1], Page: f[0], Name: feedName(names[f[0]], f[1])})
	}
	return out, len(pages), total, nil
}

// ---- pages ----

// feedPage is the data of the feed page.
type feedPage struct {
	page
	Feeds       []Feed
	Items       []Item
	Start, Next int    // the first item on the page, and the start of the next page, or 0
	Total       int    // items that match
	Filter      string // the file name of the one feed that the page shows, or ""
	FilterName  string
	Settings    feedSettings
	HasMail     bool   // the server sends email, so the digest is a choice
	Open        string // "add" when the form to follow a feed must be open
	Form        struct{ URL, Name string }
	Done        string
	Suggestions []suggestion
	Checked     int // bookmarked pages that the job looked at
	Bookmarks   int // bookmarks with a web address
}

var feedDone = map[string]string{
	"added":    "You follow the feed. Its items come with the next update, within 30 minutes.",
	"deleted":  "You do not follow the feed any more.",
	"settings": "Settings saved.",
}

// feedData gives the feed page of the account: the items from start, or
// only the items of the feed with the file name filter.
func (s *server) feedData(a *Account, filter string, start int) (feedPage, error) {
	p := feedPage{page: s.page("Your feed"), Filter: filter, Start: start, HasMail: s.mail != nil,
		Settings: s.store.view(a).Feed}
	text, err := s.store.readFeeds(a)
	if err != nil {
		return p, err
	}
	p.Feeds = parseFeeds(text)
	items := s.feedItems(a, p.Feeds)
	if filter != "" {
		items = slices.DeleteFunc(items, func(it Item) bool { return it.File != filter })
		for _, f := range p.Feeds {
			if f.File == filter {
				p.FilterName = f.Name
			}
		}
	}
	p.Total = len(items)
	if start > len(items) {
		start = len(items)
	}
	p.Items = items[start:min(start+itemsOnPage, len(items))]
	if start+itemsOnPage < len(items) {
		p.Next = start + itemsOnPage
	}
	if p.Settings.Explore {
		p.Suggestions, p.Checked, p.Bookmarks, err = s.suggestions(a, p.Feeds)
	}
	return p, err
}

// feed shows the items of the feeds of the account, newest first: from item
// ?start=, and only the feed ?feed= when that is given.
func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	u := s.owner(w, r)
	if u == nil {
		return
	}
	q := r.URL.Query()
	start, _ := strconv.Atoi(q.Get("start"))
	p, err := s.feedData(u, q.Get("feed"), max(start, 0))
	if err != nil {
		s.fail(w, err)
		return
	}
	p.Done = feedDone[q.Get("done")]
	s.render(w, http.StatusOK, "feed", p)
}

// followFeed adds the feed of the form to feeds.txt.
func (s *server) followFeed(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	raw, name := r.PostFormValue("url"), r.PostFormValue("name")
	addr, err := feedURL(raw)
	if err == nil {
		err = s.store.changeFeeds(u, func(text string) (string, error) {
			feeds := parseFeeds(text)
			if len(feeds) >= maxFeeds {
				return "", errFeedMany
			}
			for _, f := range feeds {
				if norm(f.URL) == norm(addr) {
					return "", errFeedKnown
				}
			}
			return join(append(lines(text), addr+"\t"+feedName(name, addr))), nil
		})
	}
	status := http.StatusBadRequest
	switch {
	case err == nil:
		http.Redirect(w, r, "/feed?done=added", http.StatusSeeOther)
		return
	case errors.Is(err, errFeedKnown):
		status = http.StatusConflict
	case errors.Is(err, errNoURL), errors.Is(err, errFeedURL), errors.Is(err, errFeedLong), errors.Is(err, errFeedMany):
	default:
		s.fail(w, err)
		return
	}
	p, perr := s.feedData(u, "", 0)
	if perr != nil {
		s.fail(w, perr)
		return
	}
	p.Error, p.Open, p.Form.URL, p.Form.Name = sentence(err), "add", raw, name
	s.render(w, status, "feed", p)
}

// unfollowFeed removes the feed with the URL of the form from feeds.txt.
func (s *server) unfollowFeed(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	k := norm(strings.TrimSpace(r.PostFormValue("url")))
	err := s.store.changeFeeds(u, func(text string) (string, error) {
		var out []string
		for _, l := range lines(text) {
			if k == "" || key(l) != k {
				out = append(out, l)
			}
		}
		return join(out), nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/feed?done=deleted", http.StatusSeeOther)
}

// saveFeedSettings takes the check boxes of the settings form.
func (s *server) saveFeedSettings(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	f := feedSettings{
		NewTab:  r.PostFormValue("newtab") != "",
		Mail:    s.mail != nil && r.PostFormValue("mail") != "",
		Explore: r.PostFormValue("explore") != "",
	}
	if err := s.store.setFeed(u, f); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/feed?done=settings", http.StatusSeeOther)
}

// ---- API ----

// apiOwner gives the account of the token when it can use its bookmarks
// and feeds: as for sync, its email is confirmed and sync is not paused.
// Else apiOwner answers the request and gives nil.
func (s *server) apiOwner(w http.ResponseWriter, r *http.Request) *Account {
	u := s.apiUser(r)
	if u == nil {
		http.Error(w, "not signed in: sign in again", http.StatusUnauthorized)
		return nil
	}
	if !s.verified(s.store.view(u)) {
		http.Error(w, "confirm your email address first: open the link in the email from sbm Sync, or see "+s.site+"/account", http.StatusForbidden)
		return nil
	}
	if !s.active(s.store.view(u)) {
		http.Error(w, "sbm Sync is paused for this account: see "+s.site+"/account", http.StatusPaymentRequired)
		return nil
	}
	return u
}

// apiFeed gives the items of all feeds of the account in the sfeed format,
// newest first, for the sfeed tools:
//
//	curl -H "Authorization: Bearer $token" https://sbmsync.com/api/feed | sfeed_plain
func (s *server) apiFeed(w http.ResponseWriter, r *http.Request) {
	u := s.apiOwner(w, r)
	if u == nil {
		return
	}
	text, err := s.store.readFeeds(u)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, it := range s.feedItems(u, parseFeeds(text)) {
		io.WriteString(w, it.Raw+"\n")
	}
}

// apiFeeds gives feeds.txt: the feeds that the account follows.
func (s *server) apiFeeds(w http.ResponseWriter, r *http.Request) {
	u := s.apiOwner(w, r)
	if u == nil {
		return
	}
	text, err := s.store.readFeeds(u)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, text)
}
