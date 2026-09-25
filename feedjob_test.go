package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTools are feed tools that need no sfeed: "sfeed_update" writes the
// items of a table, and "sfeed_web" answers from a table.
type fakeTools struct {
	items   map[string]string   // feed URL: the sfeed lines of the feed
	pages   map[string][]string // page URL: the "URL<tab>type" lines that sfeed_web finds
	updates []string            // the sfeedrc of each update
	crawled []string            // the pages given to discover
	plain   int                 // calls of plain
	now     time.Time
}

func (f *fakeTools) tools() feedTools {
	return feedTools{
		update: func(rc string) error {
			f.updates = append(f.updates, rc)
			b, err := os.ReadFile(rc)
			if err != nil {
				return err
			}
			dir := ""
			for _, l := range lines(string(b)) {
				if v, ok := strings.CutPrefix(l, "sfeedpath='"); ok {
					dir = strings.TrimSuffix(v, "'")
				}
				if v, ok := strings.CutPrefix(l, "\tfeed '"); ok {
					file, u, _ := strings.Cut(strings.TrimSuffix(v, "'"), "' '")
					if err := os.WriteFile(filepath.Join(dir, file), []byte(f.items[u]), 0o600); err != nil {
						return err
					}
				}
			}
			return nil
		},
		plain: func(files []string) (string, error) {
			f.plain++
			var b strings.Builder
			for _, path := range files {
				text, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				b.WriteString(filepath.Base(path) + ":\n")
				for _, l := range lines(string(text)) {
					c := strings.Split(l, "\t")
					b.WriteString("  " + c[1] + "  " + c[2] + "\n")
				}
			}
			return b.String(), nil
		},
		discover: func(page string) ([]string, error) {
			f.crawled = append(f.crawled, page)
			return f.pages[page], nil
		},
		lookup: func(host string) ([]net.IP, error) {
			switch host {
			case "internal.test":
				return []net.IP{net.ParseIP("10.66.0.4")}, nil
			case "nowhere.test":
				return nil, errors.New("no such host")
			}
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
		now: func() time.Time { return f.now },
	}
}

// fileOf gives the text of a file of the feed directory of the account, or
// "" when the file is missing.
func (e *harness) feedFile(a *Account, name string) string {
	b, err := os.ReadFile(filepath.Join(e.s.store.feedDir(a.ID), name))
	if err != nil {
		return ""
	}
	return string(b)
}

const oneItem = "1700000000\tOne\thttps://a.org/1\t\t\tid1\t\t\t\n"

func TestJobFetchesTheFeeds(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\nhttp://internal.test/rss\tInside\nhttps://nowhere.test/x\tGone\nhttps://10.0.0.7/rss\tPrivate\n")
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": oneItem}, now: time.Unix(1_800_000_000, 0)}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	rc := e.feedFile(a, "sfeedrc")
	itemsDir := filepath.Join(e.s.store.feedDir(a.ID), "items")
	// The conditional fetch (-z) is only for an item file with items in it.
	for _, want := range []string{"sfeedpath='" + itemsDir + "'\n", "\tfeed 'Alpha' 'https://a.org/rss.xml'\n",
		"\tif [ -s \"$3\" ]; then\n\t\tcurl --proto '=http,https'", ` -z "$3" "$2" 2>/dev/null` + "\n\telse\n\t\tcurl "} {
		if !strings.Contains(rc, want) {
			t.Errorf("the sfeedrc lacks %q:\n%s", want, rc)
		}
	}
	// A host on a private network, an unknown host and an address on a
	// private network stay out of the sfeedrc.
	for _, bad := range []string{"internal.test", "nowhere.test", "10.0.0.7"} {
		if strings.Contains(rc, bad) {
			t.Errorf("the sfeedrc has %q", bad)
		}
	}
	if len(f.updates) != 1 || f.updates[0] != filepath.Join(e.s.store.feedDir(a.ID), "sfeedrc") {
		t.Errorf("updates: %q", f.updates)
	}
	if got := e.feedFile(a, filepath.Join("items", "Alpha")); got != oneItem {
		t.Errorf("items of Alpha: %q", got)
	}
	if page := e.body(c, "/feed"); !strings.Contains(page, `<a href="https://a.org/1">One</a>`) {
		t.Error("the page does not show the item")
	}
	// The items of a feed that the account left go.
	e.setFeeds(tok, "")
	e.s.runFeeds()
	if e.feedFile(a, filepath.Join("items", "Alpha")) != "" || e.feedFile(a, "sfeedrc") != "" || len(f.updates) != 1 {
		t.Error("the items or the sfeedrc of the left feed stay")
	}
}

func TestJobLeavesAccountsWithoutFeedsAlone(t *testing.T) {
	e := newEnv(t, false)
	_, tok, _ := e.account("")
	a := e.s.store.byTok(tok)
	f := &fakeTools{now: time.Now()}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	if _, err := os.Stat(e.s.store.feedDir(a.ID)); !os.IsNotExist(err) {
		t.Errorf("the job made a feed directory: %v", err)
	}
}

func TestJobSendsADailyDigest(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	e.withMail()
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	start := time.Unix(1_800_000_000, 0)
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": oneItem}, now: start}
	e.s.feeds = f.tools()

	// The email is on by default. The first run notes the items and sends
	// nothing.
	e.s.runFeeds()
	if e.sent() != 0 || e.feedFile(a, "mailed") != "Alpha\tid1\n" || strings.TrimSpace(e.feedFile(a, "digest")) != "1800000000" {
		t.Fatalf("first run: %d emails, mailed %q, digest %q", e.sent(), e.feedFile(a, "mailed"), e.feedFile(a, "digest"))
	}
	f.items["https://a.org/rss.xml"] = "1700003600\tTwo\thttps://a.org/2\t\t\tid2\t\t\t\n" + oneItem
	f.now = start.Add(time.Hour)
	e.s.runFeeds()
	if e.sent() != 0 {
		t.Fatal("a digest went out within a day")
	}
	f.now = start.Add(25 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 1 {
		t.Fatalf("%d emails after a day", e.sent())
	}
	mail := e.mails[0]
	for _, want := range []string{"me@example.org\nsbm feed: 1 new item\n", "Alpha:\n  Two  https://a.org/2\n", "Your feed: " + e.s.site + "/feed\n"} {
		if !strings.Contains(mail, want) {
			t.Errorf("the digest lacks %q:\n%s", want, mail)
		}
	}
	if strings.Contains(mail, "One") {
		t.Error("the digest has the old item")
	}
	if e.feedFile(a, "mailed") != "Alpha\tid1\nAlpha\tid2\n" || strings.TrimSpace(e.feedFile(a, "digest")) != fmt.Sprint(f.now.Unix()) {
		t.Errorf("after the digest: mailed %q, digest %q", e.feedFile(a, "mailed"), e.feedFile(a, "digest"))
	}
	f.now = start.Add(50 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 1 {
		t.Error("a digest went out without new items")
	}
	f.items["https://a.org/rss.xml"] = "1700007200\tThree\thttps://a.org/3\t\t\tid3\t\t\t\n" + f.items["https://a.org/rss.xml"]
	f.now = start.Add(51 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 2 || !strings.Contains(e.mails[1], "sbm feed: 1 new item\n") || !strings.Contains(e.mails[1], "Three") {
		t.Errorf("second digest: %d emails, %q", e.sent(), e.mails[len(e.mails)-1])
	}
	// The opt-out forgets the digests, so that a later opt-in starts fresh.
	e.send(c, "/feed/settings", url.Values{})
	e.s.runFeeds()
	if e.feedFile(a, "mailed") != "" || e.feedFile(a, "digest") != "" || e.feedFile(a, "known") != "" {
		t.Error("the opt-out kept the digest files")
	}
	f.items["https://a.org/rss.xml"] = "1700010800\tFour\thttps://a.org/4\t\t\tid4\t\t\t\n" + f.items["https://a.org/rss.xml"]
	f.now = start.Add(80 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 2 {
		t.Error("a digest went out after the opt-out")
	}
	// The opt-in starts fresh: the first run notes the items only.
	e.send(c, "/feed/settings", url.Values{"mail": {"on"}})
	e.s.runFeeds()
	if e.sent() != 2 || e.feedFile(a, "mailed") == "" {
		t.Errorf("after the opt-in: %d emails, mailed %q", e.sent(), e.feedFile(a, "mailed"))
	}
}

// A digest that does not go out is sent again with the next run.
func TestJobKeepsTheItemsOfAFailedDigest(t *testing.T) {
	e := newEnv(t, false)
	_, tok, _ := e.account("")
	e.withMail()
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	start := time.Unix(1_800_000_000, 0)
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": ""}, now: start}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	f.items["https://a.org/rss.xml"] = oneItem
	f.now = start.Add(25 * time.Hour)
	e.s.mail = func(string, string, string) error { return errors.New("no SMTP server") }
	e.s.runFeeds()
	if e.feedFile(a, "mailed") != "" || strings.TrimSpace(e.feedFile(a, "digest")) != "1800000000" {
		t.Errorf("a failed digest changed the files: mailed %q, digest %q", e.feedFile(a, "mailed"), e.feedFile(a, "digest"))
	}
	e.withMail()
	e.s.runFeeds()
	if e.sent() != 1 || !strings.Contains(e.mails[0], "One") {
		t.Errorf("the next run: %d emails", e.sent())
	}
}

// A feed that the account follows after the first digest run does not
// bring its old items into the digest: the first run that finds items of
// the feed notes them. The items that come after that go out.
func TestJobKeepsTheOldItemsOfANewFeedOutOfTheDigest(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("")
	e.withMail()
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	start := time.Unix(1_800_000_000, 0)
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": oneItem}, now: start}
	e.s.feeds = f.tools()
	e.s.runFeeds()

	// The first fetch of Beta gives its old items. The first fetch of Gamma
	// gives nothing, as a fetch that failed.
	old := "1600000000\tOld 1\thttps://b.org/1\t\t\tb1\t\t\t\n1600000001\tOld 2\thttps://b.org/2\t\t\tb2\t\t\t\n"
	f.items["https://b.org/rss.xml"] = old
	f.items["https://c.org/rss.xml"] = ""
	e.send(c, "/feed/add", url.Values{"url": {"https://b.org/rss.xml"}, "name": {"Beta"}})
	e.send(c, "/feed/add", url.Values{"url": {"https://c.org/rss.xml"}, "name": {"Gamma"}})
	f.now = start.Add(time.Hour)
	e.s.runFeeds()
	// Now the fetch of Gamma works, and Beta has a new item.
	f.items["https://c.org/rss.xml"] = "1600000002\tOld 3\thttps://c.org/3\t\t\tc3\t\t\t\n"
	f.items["https://b.org/rss.xml"] = "1800003600\tNew\thttps://b.org/new\t\t\tbnew\t\t\t\n" + old
	f.now = start.Add(2 * time.Hour)
	e.s.runFeeds()
	f.now = start.Add(25 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 1 {
		t.Fatalf("%d emails after a day", e.sent())
	}
	mail := e.mails[0]
	if !strings.Contains(mail, "sbm feed: 1 new item\n") || !strings.Contains(mail, "New") {
		t.Errorf("the digest lacks the new item:\n%s", mail)
	}
	for _, item := range []string{"Old 1", "Old 2", "Old 3"} {
		if strings.Contains(mail, item) {
			t.Errorf("the digest has the old item %q:\n%s", item, mail)
		}
	}
	if got := e.feedFile(a, "known"); got != "https://a.org/rss.xml\tAlpha\nhttps://b.org/rss.xml\tBeta\nhttps://c.org/rss.xml\tGamma\n" {
		t.Errorf("known: %q", got)
	}

	// A feed that the account left and follows again is a new feed: the
	// item that came while the account did not follow it is old.
	e.send(c, "/feed/delete", url.Values{"url": {"https://b.org/rss.xml"}})
	f.now = start.Add(26 * time.Hour)
	e.s.runFeeds()
	f.items["https://b.org/rss.xml"] = "1800090000\tWhile away\thttps://b.org/away\t\t\tbaway\t\t\t\n" + f.items["https://b.org/rss.xml"]
	e.send(c, "/feed/add", url.Values{"url": {"https://b.org/rss.xml"}, "name": {"Beta"}})
	f.now = start.Add(27 * time.Hour)
	e.s.runFeeds()
	// Under another name, the items of the feed get other keys. So the
	// feed is new, also without a run between the two changes.
	e.send(c, "/feed/delete", url.Values{"url": {"https://b.org/rss.xml"}})
	e.send(c, "/feed/add", url.Values{"url": {"https://b.org/rss.xml"}, "name": {"Bee"}})
	f.now = start.Add(28 * time.Hour)
	e.s.runFeeds()
	f.now = start.Add(50 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 1 {
		t.Errorf("old items of the feed that came back went out:\n%s", e.mails[len(e.mails)-1])
	}
	if got := e.feedFile(a, "known"); got != "https://a.org/rss.xml\tAlpha\nhttps://c.org/rss.xml\tGamma\nhttps://b.org/rss.xml\tBee\n" {
		t.Errorf("known at the end: %q", got)
	}
}

// A feed whose host does not resolve in one run stays known. So its new
// items still go out.
func TestJobKeepsAFeedKnownWhenItsHostFailsOnce(t *testing.T) {
	e := newEnv(t, false)
	_, tok, _ := e.account("")
	e.withMail()
	e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	start := time.Unix(1_800_000_000, 0)
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": oneItem}, now: start}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	e.s.feeds.lookup = func(string) ([]net.IP, error) { return nil, errors.New("no such host") }
	f.now = start.Add(time.Hour)
	e.s.runFeeds()
	e.s.feeds = f.tools()
	f.items["https://a.org/rss.xml"] = "1700003600\tTwo\thttps://a.org/2\t\t\tid2\t\t\t\n" + oneItem
	f.now = start.Add(25 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 1 || !strings.Contains(e.mails[0], "Two") {
		t.Errorf("the new item did not go out: %d emails", e.sent())
	}
}

// After an update from a version without the file known, the digest knows
// all feeds that the account follows. So no item that waits for the next
// digest is lost.
func TestJobKnowsTheFeedsOfAnOlderDigest(t *testing.T) {
	e := newEnv(t, false)
	_, tok, _ := e.account("")
	e.withMail()
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	start := time.Unix(1_800_000_000, 0)
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": oneItem}, now: start}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	if err := os.Remove(filepath.Join(e.s.store.feedDir(a.ID), "known")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	f.items["https://a.org/rss.xml"] = "1700003600\tTwo\thttps://a.org/2\t\t\tid2\t\t\t\n" + oneItem
	f.now = start.Add(25 * time.Hour)
	e.s.runFeeds()
	if e.sent() != 1 || !strings.Contains(e.mails[0], "Two") {
		t.Errorf("the update lost the new item: %d emails", e.sent())
	}
	if got := e.feedFile(a, "known"); got != "https://a.org/rss.xml\tAlpha\n" {
		t.Errorf("known: %q", got)
	}
}

func TestJobMailsOnlyConfirmedAccounts(t *testing.T) {
	e := newEnv(t, false)
	e.withMail()
	e.signup("me@example.org")
	tok := e.token("me@example.org")
	a := e.setFeeds(tok, "https://a.org/rss.xml\tAlpha\n")
	f := &fakeTools{items: map[string]string{"https://a.org/rss.xml": oneItem}, now: time.Now()}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	// The one email is the email check of the sign-up.
	if e.sent() != 1 || e.feedFile(a, "mailed") != "" {
		t.Errorf("an unconfirmed account got a digest: %d emails, mailed %q", e.sent(), e.feedFile(a, "mailed"))
	}
	if e.feedFile(a, filepath.Join("items", "Alpha")) != oneItem {
		t.Error("the feed of an unconfirmed account is not fetched")
	}
}

func TestJobExploresTheBookmarks(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("https://old.org\tOld\t\nhttps://blog.org\tBlog\t\nhttp://internal.test/\tInside\t\njavascript:x\tBad\t\n")
	a := e.s.store.byTok(tok)
	f := &fakeTools{now: time.Now(), pages: map[string][]string{
		"https://blog.org": {"https://blog.org/feed.xml\tapplication/rss+xml", "https://blog.org/it's\tapplication/atom+xml"},
	}}
	e.s.feeds = f.tools()
	e.s.runFeeds()
	if len(f.crawled) != 0 || e.feedFile(a, "explore") != "" {
		t.Fatal("the job looked at the bookmarks without the opt-in")
	}
	e.send(c, "/feed/settings", url.Values{"explore": {"on"}})
	e.s.runFeeds()
	// Newest first. The page on a private network is not fetched.
	if want := []string{"https://blog.org", "https://old.org"}; strings.Join(f.crawled, " ") != strings.Join(want, " ") {
		t.Errorf("crawled %q, want %q", f.crawled, want)
	}
	want := "http://internal.test/\t-\t\nhttps://blog.org\thttps://blog.org/feed.xml\tapplication/rss+xml\nhttps://old.org\t-\t\n"
	if got := e.feedFile(a, "explore"); got != want {
		t.Errorf("explore:\n%q\nwant\n%q", got, want)
	}
	page := e.body(c, "/feed")
	for _, want := range []string{"looked at 3 of your 3 bookmarked pages", `value="https://blog.org/feed.xml"`, `value="Blog"`, "on https://blog.org"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// The next run looks at no page twice.
	e.s.runFeeds()
	if len(f.crawled) != 2 {
		t.Errorf("crawled %q", f.crawled)
	}
	// A feed that the account follows is no suggestion any more.
	e.send(c, "/feed/add", url.Values{"url": {"https://blog.org/feed.xml"}, "name": {"Blog"}})
	_, explore, _ := strings.Cut(e.body(c, "/feed"), "<h2>Explore</h2>")
	if strings.Contains(explore, "feed.xml") || !strings.Contains(explore, "No new feed to follow yet.") {
		t.Errorf("the page still suggests the followed feed:\n%s", explore)
	}
}

func TestJobExploresTwentyPagesInOneRun(t *testing.T) {
	e := newEnv(t, false)
	var b strings.Builder
	for i := range crawlPerRun + 5 {
		fmt.Fprintf(&b, "https://p%d.org\tP%d\t\n", i, i)
	}
	c, tok, _ := e.account(b.String())
	f := &fakeTools{now: time.Now()}
	e.s.feeds = f.tools()
	e.send(c, "/feed/settings", url.Values{"explore": {"on"}})
	e.s.runFeeds()
	if len(f.crawled) != crawlPerRun || f.crawled[0] != "https://p24.org" {
		t.Errorf("first run: %d pages, first %q", len(f.crawled), f.crawled[0])
	}
	if !strings.Contains(e.body(c, "/feed"), "looked at 20 of your 25 bookmarked pages") {
		t.Error("the page does not show the count")
	}
	e.s.runFeeds()
	if len(f.crawled) != crawlPerRun+5 {
		t.Errorf("second run: %d pages", len(f.crawled))
	}
	_ = tok
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"93.184.216.34": true, "2606:2800:220:1:248:1893:25c8:1946": true,
		"127.0.0.1": false, "10.66.0.4": false, "192.168.1.1": false, "172.16.0.9": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
