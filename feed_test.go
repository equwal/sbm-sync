package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// setFeeds writes text as feeds.txt of the account of the token.
func (e *harness) setFeeds(tok, text string) *Account {
	a := e.s.store.byTok(tok)
	if err := e.s.store.changeFeeds(a, func(string) (string, error) { return text, nil }); err != nil {
		e.t.Fatal(err)
	}
	return a
}

// setItems writes an sfeed file for the feed with the file name.
func (e *harness) setItems(a *Account, file, text string) {
	dir := filepath.Join(e.s.store.feedDir(a.ID), "items")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// itemTitles gives the titles of the items on the feed page, without the
// lists of feeds and suggestions under them.
func itemTitles(page string) []string {
	head, _, _ := strings.Cut(page, "<h2>")
	return titles(head)
}

// feedsOf gives feeds.txt as the API gives it.
func (e *harness) feedsOf(tok string) string {
	code, body := e.api("GET", tok, "/api/feeds", nil)
	if code != http.StatusOK {
		e.t.Fatalf("GET /api/feeds: %d %s", code, body)
	}
	return body
}

func TestFeedPagesNeedSignIn(t *testing.T) {
	e := newEnv(t, false)
	c := e.browser()
	if code, to, _ := e.get(c, "/feed"); code != http.StatusSeeOther || to != "/login" {
		t.Errorf("GET /feed: %d to %q", code, to)
	}
	for _, path := range []string{"/feed/add", "/feed/delete", "/feed/settings"} {
		if code, to, _ := e.send(c, path, url.Values{"url": {"https://a.org/rss.xml"}}); code != http.StatusSeeOther || to != "/login" {
			t.Errorf("POST %s: %d to %q", path, code, to)
		}
	}
	for _, path := range []string{"/api/feed", "/api/feeds"} {
		if code, _ := e.api("GET", "wrong", path, nil); code != http.StatusUnauthorized {
			t.Errorf("GET %s with a bad token: %d", path, code)
		}
	}
}

func TestFeedWaitsForTheEmailCheck(t *testing.T) {
	e := newEnv(t, false)
	e.withMail()
	c := e.signup("me@example.org")
	tok := e.token("me@example.org")
	if code, to, _ := e.get(c, "/feed"); code != http.StatusSeeOther || to != "/account" {
		t.Errorf("feed before the check: %d to %q", code, to)
	}
	if code, _ := e.api("GET", tok, "/api/feed", nil); code != http.StatusForbidden {
		t.Errorf("API before the check: %d", code)
	}
	e.open(e.link())
	if code, _, _ := e.get(c, "/feed"); code != http.StatusOK {
		t.Errorf("feed after the check: %d", code)
	}
	if code, _ := e.api("GET", tok, "/api/feed", nil); code != http.StatusOK {
		t.Errorf("API after the check: %d", code)
	}
}

func TestFollowAndUnfollow(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	code, to, _ := e.send(c, "/feed/add", url.Values{"url": {" https://a.org/rss.xml "}, "name": {" A\tfeed "}})
	if code != http.StatusSeeOther || to != "/feed?done=added" {
		t.Fatalf("follow: %d to %q", code, to)
	}
	if got := e.feedsOf(tok); got != "https://a.org/rss.xml\tA feed\n" {
		t.Fatalf("feeds.txt: %q", got)
	}
	if !strings.Contains(e.body(c, to), "You follow the feed.") {
		t.Error("the page does not tell that the feed is followed")
	}
	code, _, page := e.send(c, "/feed/add", url.Values{"url": {"http://www.A.org/rss.xml/"}, "name": {"again"}})
	if code != http.StatusConflict || !strings.Contains(page, "You follow this feed already.") || !strings.Contains(page, "<details open>") || !strings.Contains(page, `value="again"`) {
		t.Errorf("follow the same feed: %d\n%s", code, page)
	}
	// Without a name, the host is the name.
	e.send(c, "/feed/add", url.Values{"url": {"https://b.org/atom"}})
	want := "https://a.org/rss.xml\tA feed\nhttps://b.org/atom\tb.org\n"
	if got := e.feedsOf(tok); got != want {
		t.Fatalf("feeds.txt: %q, want %q", got, want)
	}
	for _, bad := range []string{"javascript:alert(1)", "https://a.org/it's", "https://a.org/x y", "ftp://a.org/f", "a.org/rss", "", "https://" + strings.Repeat("a", 2100)} {
		if code, _, _ := e.send(c, "/feed/add", url.Values{"url": {bad}}); code != http.StatusBadRequest {
			t.Errorf("follow %q: %d", bad, code)
		}
	}
	if got := e.feedsOf(tok); got != want {
		t.Fatalf("a refusal changed feeds.txt: %q", got)
	}
	if code, to, _ := e.send(c, "/feed/delete", url.Values{"url": {"https://A.org/rss.xml/"}}); code != http.StatusSeeOther || to != "/feed?done=deleted" {
		t.Fatalf("unfollow: %d to %q", code, to)
	}
	if got := e.feedsOf(tok); got != "https://b.org/atom\tb.org\n" {
		t.Fatalf("feeds.txt after the unfollow: %q", got)
	}
}

// A new account follows the default feeds of the server. It can unfollow
// them, and they do not come back.
func TestDefaultFeeds(t *testing.T) {
	e := newEnv(t, false)
	e.s.store.defaultFeeds = "https://recentlywritten.com/rss.xml\trecentlywritten.com\n"
	c, tok, _ := e.account("")
	a := e.s.store.byTok(tok)
	if got := e.feedsOf(tok); got != e.s.store.defaultFeeds {
		t.Fatalf("feeds of a new account: %q", got)
	}
	if !strings.Contains(e.body(c, "/feed"), `<a href="/feed?feed=recentlywritten.com">recentlywritten.com</a>`) {
		t.Error("the page does not show the default feed")
	}
	// The job fetches the default feed of an account that never opened the page.
	f := &fakeTools{items: map[string]string{"https://recentlywritten.com/rss.xml": oneItem}, now: time.Now()}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	if !strings.Contains(e.feedFile(a, "sfeedrc"), "'https://recentlywritten.com/rss.xml'") {
		t.Error("the job does not fetch the default feed")
	}
	e.send(c, "/feed/add", url.Values{"url": {"https://a.org/rss.xml"}, "name": {"A"}})
	if got := e.feedsOf(tok); got != e.s.store.defaultFeeds+"https://a.org/rss.xml\tA\n" {
		t.Fatalf("after a follow: %q", got)
	}
	e.send(c, "/feed/delete", url.Values{"url": {"https://recentlywritten.com/rss.xml"}})
	if got := e.feedsOf(tok); got != "https://a.org/rss.xml\tA\n" {
		t.Fatalf("after the unfollow of the default feed: %q", got)
	}
	e.send(c, "/feed/delete", url.Values{"url": {"https://a.org/rss.xml"}})
	if got := e.feedsOf(tok); got != "" {
		t.Fatalf("after the unfollow of all feeds, the default is back: %q", got)
	}
	e.s.runFeeds()
	if got := e.feedsOf(tok); got != "" {
		t.Fatalf("the job brought the default feed back: %q", got)
	}
}

func TestDefaultFeedsSetting(t *testing.T) {
	got, err := defaultFeeds(" https://recentlywritten.com/rss.xml\nhttp://a.org/feed ")
	if err != nil || got != "https://recentlywritten.com/rss.xml\trecentlywritten.com\nhttp://a.org/feed\ta.org\n" {
		t.Errorf("defaultFeeds = %q, %v", got, err)
	}
	if got, err := defaultFeeds(""); err != nil || got != "" {
		t.Errorf("empty setting: %q, %v", got, err)
	}
	for _, bad := range []string{"ftp://a.org/feed", "https://a.org/it's", "a.org/feed"} {
		if _, err := defaultFeeds(bad); err == nil {
			t.Errorf("defaultFeeds(%q) gave no error", bad)
		}
	}
}

func TestFeedLimit(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	var b strings.Builder
	for i := range maxFeeds {
		fmt.Fprintf(&b, "https://f%d.org/rss\tF%d\n", i, i)
	}
	e.setFeeds(tok, b.String())
	if code, _, page := e.send(c, "/feed/add", url.Values{"url": {"https://one-more.org/rss"}}); code != http.StatusBadRequest || !strings.Contains(page, "200 feeds already") {
		t.Errorf("feed 201: %d", code)
	}
}

func TestFeedTimeline(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\nhttps://b.org/feed\tBeta & Co\n")
	e.setItems(a, "Alpha", "1700000000\tOld \\t tab\thttps://a.org/1\t\t\tid1\tAnn\t\t\n1700003600\tNewer\thttps://a.org/2\t\t\t\t\t\t\n")
	e.setItems(a, "Beta-Co", "1700001800\tScript <b>\tjavascript:alert(1)\t\t\tid3\t\t\t\n\tNo time\thttps://b.org/4\t\t\tid4\t\t\t\n")
	page := e.body(c, "/feed")
	if got := itemTitles(page); !slices.Equal(got, []string{"Newer", "Script &lt;b&gt;", "Old \t tab", "No time"}) {
		t.Fatalf("titles: %q\n%s", got, page)
	}
	for _, want := range []string{`<a href="https://a.org/2">Newer</a>`, "Beta &amp; Co", " · Ann", "2023-11-14 23:13", "4 items."} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, bad := range []string{`href="javascript:`, `target="_blank"`} {
		if strings.Contains(page, bad) {
			t.Errorf("the page has %q", bad)
		}
	}
	page = e.body(c, "/feed?feed=Beta-Co")
	if got := itemTitles(page); !slices.Equal(got, []string{"Script &lt;b&gt;", "No time"}) || !strings.Contains(page, "from <b>Beta &amp; Co</b>") {
		t.Errorf("one feed: %q", got)
	}
	if page := e.body(c, "/feed?feed=nothing"); !strings.Contains(page, "0 items.") {
		t.Error("an unknown feed shows items")
	}

	if code, to, _ := e.send(c, "/feed/settings", url.Values{"newtab": {"on"}}); code != http.StatusSeeOther || to != "/feed?done=settings" {
		t.Fatalf("settings: %d to %q", code, to)
	}
	if page := e.body(c, "/feed"); !strings.Contains(page, `<a href="https://a.org/2" target="_blank" rel="noopener">Newer</a>`) {
		t.Error("links do not open in a new tab")
	}
	if !e.s.store.view(a).Feed.NewTab {
		t.Error("the setting is not kept")
	}
	e.send(c, "/feed/settings", url.Values{})
	if page := e.body(c, "/feed"); strings.Contains(page, `target="_blank"`) || e.s.store.view(a).Feed.NewTab {
		t.Error("links still open in a new tab")
	}

	code, body := e.api("GET", tok, "/api/feed", nil)
	want := "1700003600\tNewer\thttps://a.org/2\t\t\t\t\t\t\n" +
		"1700001800\tScript <b>\tjavascript:alert(1)\t\t\tid3\t\t\t\n" +
		"1700000000\tOld \\t tab\thttps://a.org/1\t\t\tid1\tAnn\t\t\n" +
		"\tNo time\thttps://b.org/4\t\t\tid4\t\t\t\n"
	if code != http.StatusOK || body != want {
		t.Errorf("GET /api/feed: %d\n%q\nwant\n%q", code, body, want)
	}
}

func TestFeedPages(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	var b strings.Builder
	for i := range itemsOnPage + 5 {
		fmt.Fprintf(&b, "%d\tn%d\thttps://a.org/%d\t\t\t\t\t\t\n", 1700000000+i, i, i)
	}
	e.setItems(a, "Alpha", b.String())
	page := e.body(c, "/feed")
	if got := itemTitles(page); len(got) != itemsOnPage || got[0] != "n104" || !strings.Contains(page, `<a href="/feed?start=100">Older items</a>`) {
		t.Errorf("page 1: %d titles, first %q", len(got), got[0])
	}
	page = e.body(c, "/feed?start=100&feed=Alpha")
	if got := itemTitles(page); !slices.Equal(got, []string{"n4", "n3", "n2", "n1", "n0"}) || strings.Contains(page, "Older items") {
		t.Errorf("page 2: %q", got)
	}
}

// The daily email is on by default. The page offers the box only when the
// server sends email, and a save without the box keeps the choice.
func TestFeedMailSettingNeedsAnSMTPServer(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	a := e.s.store.byTok(tok)
	if strings.Contains(e.body(c, "/feed"), `name="mail"`) {
		t.Error("the page offers the digest without an SMTP server")
	}
	e.send(c, "/feed/settings", url.Values{"explore": {"on"}})
	if f := e.s.store.view(a).Feed; f.NoMail || !f.Explore {
		t.Errorf("settings without an SMTP server: %+v", f)
	}
	e.withMail()
	if !strings.Contains(e.body(c, "/feed"), `<input type="checkbox" class="check" name="mail" checked>`) {
		t.Error("the page does not offer the digest, on")
	}
	e.send(c, "/feed/settings", url.Values{})
	if f := e.s.store.view(a).Feed; !f.NoMail || f.Explore {
		t.Errorf("settings with the box cleared: %+v", f)
	}
	if !strings.Contains(e.body(c, "/feed"), `<input type="checkbox" class="check" name="mail">`) {
		t.Error("the page shows the digest on")
	}
	e.send(c, "/feed/settings", url.Values{"mail": {"on"}})
	if f := e.s.store.view(a).Feed; f.NoMail {
		t.Errorf("settings with the box ticked: %+v", f)
	}
}

func TestOtherSiteCannotChangeFeeds(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	for _, path := range []string{"/feed/add", "/feed/delete", "/feed/settings"} {
		req, _ := http.NewRequest("POST", e.web.URL+path, strings.NewReader("url=https%3A%2F%2Fa.org%2Frss.xml&newtab=on"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://evil.example")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s from another site: %s", path, resp.Status)
		}
	}
	if got := e.feedsOf(tok); got != "https://a.org/rss.xml\tAlpha\n" {
		t.Errorf("another site changed the feeds: %q", got)
	}
}

func TestDeleteAccountRemovesTheFeeds(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	if resp := e.post(c, "/account/delete", url.Values{"password": {"password1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %s", resp.Status)
	}
	if _, err := os.Stat(e.s.store.feedDir(a.ID)); !os.IsNotExist(err) {
		t.Errorf("the feeds of the account are still there: %v", err)
	}
}

// feedURLs draws strings that are often feed URLs and often not.
var feedURLs = rapid.Custom(func(t *rapid.T) string {
	head := rapid.SampledFrom([]string{"https://a.org/", "http://b.org", "HTTPS://c.org/x", "https://", "ftp://c.org/", "javascript:", "", " https://d.org/"}).Draw(t, "head")
	tail := rapid.StringOf(rapid.SampledFrom([]rune("abcz09:/.-_?=&%#~'\" \t\n\\日é\x01"))).Draw(t, "tail")
	return head + tail
})

// A URL that feedURL takes has only printable ASCII without single quotes,
// so that it holds between single quotes in the sfeedrc. Each such http or
// https URL with a host is taken.
func TestFeedURLIsShellSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		raw := feedURLs.Draw(t, "url")
		u := strings.TrimSpace(raw)
		safe := u != "" && len(u) <= maxFeedURL
		for i := 0; i < len(u); i++ {
			if c := u[i]; c <= ' ' || c > '~' || c == '\'' {
				safe = false
			}
		}
		p, err := url.Parse(u)
		want := safe && err == nil && p.Host != "" && (p.Scheme == "http" || p.Scheme == "https")
		got, err := feedURL(raw)
		if (err == nil) != want {
			t.Fatalf("feedURL(%q) = %q, %v; want ok %v", raw, got, err, want)
		}
		if err != nil {
			return
		}
		if got != u {
			t.Fatalf("feedURL(%q) = %q, want %q", raw, got, u)
		}
		rc := sfeedrc("/data/items", []Feed{{URL: got, Name: "x", File: "x"}})
		line := "\tfeed 'x' '" + got + "'\n"
		if !strings.Contains(rc, line) || strings.Count(line, "'") != 4 {
			t.Fatalf("sfeedrc for %q:\n%s", got, rc)
		}
	})
}

var fileNames = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// Each line of feeds.txt parses back, and each feed gets a safe file name
// that no other feed of the file has.
func TestFeedsParseBackWithUniqueFiles(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var b strings.Builder
		var want []Feed
		for i := range rapid.IntRange(0, 6).Draw(t, "feeds") {
			u := fmt.Sprintf("https://f%d.org/rss", i)
			name := feedName(rapid.StringOf(rapid.SampledFrom([]rune("aA0 -._/\t\n日'"))).Draw(t, "name"), u)
			if strings.ContainsAny(name, "\t\n") || len([]rune(name)) > maxFeedName {
				t.Fatalf("feedName gave %q", name)
			}
			b.WriteString(u + "\t" + name + "\n")
			want = append(want, Feed{URL: u, Name: name})
		}
		got := parseFeeds(b.String())
		if len(got) != len(want) {
			t.Fatalf("%d feeds parsed, want %d", len(got), len(want))
		}
		files := map[string]bool{}
		for i, f := range got {
			if f.URL != want[i].URL || f.Name != want[i].Name {
				t.Fatalf("feed %d is %+v, want %+v", i, f, want[i])
			}
			if !fileNames.MatchString(f.File) || files[f.File] || len(f.File) > maxFileName+4 {
				t.Fatalf("feed %d has the file name %q; files %v", i, f.File, files)
			}
			files[f.File] = true
		}
	})
}

// escape is the escaping of the sfeed format, as a reference.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`).Replace(s)
}

func TestUnescapeUndoesEscape(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringOf(rapid.SampledFrom([]rune("ab\\tn\t\n日"))).Draw(t, "text")
		if got := unescape(escape(s)); got != s {
			t.Fatalf("unescape(%q) = %q, want %q", escape(s), got, s)
		}
	})
}

func TestParseItems(t *testing.T) {
	f := Feed{URL: "https://a.org/rss", Name: "A", File: "A"}
	items := parseItems("1700000000\tT\\ta\\\\b\\n\thttps://a.org/1\tbody\thtml\tid1\tAnn\t\tcat\n"+
		"x\t\thttps://a.org/2\t\t\t\t\t\n"+
		"short\n"+
		"1\t\t\t\t\t\t\t\t\n", f)
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}
	a, b, c := items[0], items[1], items[2]
	if a.Title != "T\ta\\b\n" || a.Link != "https://a.org/1" || a.Author != "Ann" || a.Key != "A\tid1" || a.Time.Unix() != 1700000000 || a.Feed != "A" {
		t.Errorf("item 1: %+v", a)
	}
	// Without a time, an id or a title: the time is zero, the link stands in
	// for the id, and the title comes from the link.
	if !b.Time.IsZero() || b.Key != "A\thttps://a.org/2" || b.Title != "https://a.org/2" {
		t.Errorf("item 2: %+v", b)
	}
	if c.Title != "(no title)" || c.Key != "A\t" || c.Link != "" {
		t.Errorf("item 3: %+v", c)
	}
}
