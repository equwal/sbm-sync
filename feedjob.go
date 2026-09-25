package main

// The feed job: `sbm-sync feed`. A systemd timer runs it every 30 minutes
// (contrib/sbm-feed.timer). For each account it:
//
//  1. writes an sfeedrc from feeds.txt and runs sfeed_update, which fetches
//     each feed and merges the new items into items/<file>;
//  2. when the account asked for it, looks through the bookmarks of the
//     account for feeds with sfeed_web, and keeps what it found in explore;
//  3. when the account asked for it, sends the items that are new since the
//     last digest by email, once a day, in the form of sfeed_plain.
//
// The job writes only its own files under feeds/<account>/. The web server
// writes only feeds.txt and accounts.json. So the two never write the same
// file.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	digestEvery  = 24 * time.Hour
	crawlPerRun  = 20 // bookmarked pages that explore looks at in one run
	feedsPerPage = 5  // feeds that explore keeps from one page
	updateTime   = 10 * time.Minute
)

// feedTools are the programs and the clock that the job uses. Tests
// replace them.
type feedTools struct {
	update   func(sfeedrc string) error           // runs sfeed_update
	plain    func(files []string) (string, error) // runs sfeed_plain on sfeed files
	discover func(page string) ([]string, error)  // gives "URL<tab>type" for each feed of a page, as curl | sfeed_web
	lookup   func(host string) ([]net.IP, error)  // resolves a host name
	now      func() time.Time
}

// execTools gives the tools that run the programs.
func execTools() feedTools {
	return feedTools{
		update: func(sfeedrc string) error {
			ctx, cancel := context.WithTimeout(context.Background(), updateTime)
			defer cancel()
			out, err := exec.CommandContext(ctx, "sfeed_update", sfeedrc).CombinedOutput()
			for _, l := range lines(string(out)) {
				log.Print(l)
			}
			return err
		},
		plain: func(files []string) (string, error) {
			out, err := exec.Command("sfeed_plain", files...).Output()
			return string(out), err
		},
		discover: func(page string) ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			curl := exec.CommandContext(ctx, "curl", "-sfL", "--proto", "=http,https", "--max-redirs", "3",
				"-m", "10", "--max-filesize", "1048576", "-A", "sbm-sync (+https://github.com/equwal/sbm-sync)", page)
			web := exec.CommandContext(ctx, "sfeed_web", page)
			pipe, err := curl.StdoutPipe()
			if err != nil {
				return nil, err
			}
			web.Stdin = pipe
			if err := curl.Start(); err != nil {
				return nil, err
			}
			out, err := web.Output()
			curl.Wait() // a page that does not load gives no feeds
			if err != nil {
				return nil, err
			}
			return lines(string(out)), nil
		},
		lookup: net.LookupIP,
		now:    time.Now,
	}
}

// runFeeds does the work of the job for every account.
func (s *server) runFeeds() {
	accounts := s.store.all()
	for _, a := range accounts {
		if err := s.feedAccount(a); err != nil {
			log.Printf("account %s: %v", a.ID, err)
		}
	}
	log.Printf("feeds of %d accounts done", len(accounts))
}

// sfeedrc gives the sfeed_update settings for the feeds: the item files go
// to itemsDir. Each URL passed feedURL, and each file name has only safe
// characters, so the single quotes hold.
//
// The fetch asks only for a feed that changed since the item file was
// written (-z), but not when that file is empty: sfeed_update touches the
// file before the first fetch, and curl would then skip the whole feed as
// not newer than the file.
func sfeedrc(itemsDir string, feeds []Feed) string {
	const curl = "curl --proto '=http,https' -L --max-redirs 3 -A 'sbm-sync (+https://github.com/equwal/sbm-sync)' -f -s -m 20 --max-filesize 5242880"
	var b strings.Builder
	b.WriteString("# sbm-sync makes this file from feeds.txt. Do not edit it.\n")
	b.WriteString("sfeedpath='" + itemsDir + "'\n")
	b.WriteString("maxjobs=4\n")
	b.WriteString("fetch() {\n\tif [ -s \"$3\" ]; then\n\t\t" + curl + " -z \"$3\" \"$2\" 2>/dev/null\n\telse\n\t\t" + curl + " \"$2\" 2>/dev/null\n\tfi\n}\n")
	b.WriteString("feeds() {\n")
	for _, f := range feeds {
		b.WriteString("\tfeed '" + f.File + "' '" + f.URL + "'\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// publicIP tells if an address is on the public internet.
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	// 100.64.0.0/10 is for carrier networks, as private as 10.0.0.0/8.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return false
	}
	return true
}

// public tells if the host of the URL is on the public internet. The job
// fetches only such hosts, so that a feed URL cannot reach the services on
// this machine or in its private networks. cache keeps the answers of one
// run.
func (s *server) public(rawURL string, cache map[string]bool) bool {
	p, err := url.Parse(rawURL)
	if err != nil || p.Hostname() == "" {
		return false
	}
	host := p.Hostname()
	if ok, seen := cache[host]; seen {
		return ok
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if r, err := s.feeds.lookup(host); err == nil {
		ips = r
	}
	ok := len(ips) > 0
	for _, ip := range ips {
		if !publicIP(ip) {
			ok = false
		}
	}
	cache[host] = ok
	return ok
}

// feedAccount does the work of the job for one account.
func (s *server) feedAccount(p *Account) error {
	a := s.store.view(p)
	dir := s.store.feedDir(a.ID)
	itemsDir := filepath.Join(dir, "items")
	text, err := s.store.readFeeds(p)
	if err != nil {
		return err
	}
	cache := map[string]bool{}
	var feeds []Feed
	for _, f := range parseFeeds(text) {
		if _, err := feedURL(f.URL); err != nil || !s.public(f.URL, cache) {
			log.Printf("account %s: feed %s: not a public http address, skipped", a.ID, f.URL)
			continue
		}
		feeds = append(feeds, f)
	}
	if len(feeds) == 0 && !a.Feed.Explore && !a.Feed.Mail {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil // the account never used feeds
		}
	}
	if err := os.MkdirAll(itemsDir, 0o700); err != nil {
		return err
	}
	rcPath := filepath.Join(dir, "sfeedrc")
	if len(feeds) > 0 {
		rc := sfeedrc(itemsDir, feeds)
		if old, _ := os.ReadFile(rcPath); string(old) != rc {
			if err := writeFile(rcPath, []byte(rc)); err != nil {
				return err
			}
		}
		if err := s.feeds.update(rcPath); err != nil {
			log.Printf("account %s: sfeed_update: %v", a.ID, err)
		}
	} else {
		os.Remove(rcPath)
	}
	// The items of feeds that the account left go.
	keep := map[string]bool{}
	for _, f := range feeds {
		keep[f.File] = true
	}
	entries, err := os.ReadDir(itemsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			os.Remove(filepath.Join(itemsDir, e.Name()))
		}
	}
	if a.Feed.Explore {
		if err := s.explore(p, dir, cache); err != nil {
			log.Printf("account %s: explore: %v", a.ID, err)
		}
	}
	if !a.Feed.Mail {
		os.Remove(filepath.Join(dir, "mailed"))
		os.Remove(filepath.Join(dir, "digest"))
	} else if s.mail != nil && s.verified(a) && s.active(a) {
		if err := s.digest(a, dir, feeds); err != nil {
			log.Printf("account %s: digest: %v", a.ID, err)
		}
	}
	return nil
}

// explore looks at up to crawlPerRun bookmarked pages that it did not look
// at before, newest first, and adds the feeds that it finds to the explore
// file. A page without a feed, or one that the job cannot fetch, gets a
// line with "-", so that the job does not look at it again.
func (s *server) explore(p *Account, dir string, cache map[string]bool) error {
	text, _, err := s.store.read(p, "")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "explore")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	seen := map[string]bool{}
	for _, l := range lines(string(old)) {
		page, _, _ := strings.Cut(l, "\t")
		seen[page] = true
	}
	var add []string
	n := 0
	for _, b := range slices.Backward(parse(text)) {
		if n >= crawlPerRun {
			break
		}
		link := b.Link()
		if link == "" || seen[link] {
			continue
		}
		seen[link] = true
		if _, err := feedURL(link); err != nil || !s.public(link, cache) {
			add = append(add, link+"\t-\t")
			continue
		}
		n++
		found, err := s.feeds.discover(link)
		if err != nil {
			log.Printf("account %s: explore %s: %v", p.ID, link, err)
		}
		kept := 0
		for _, f := range found {
			u, typ, _ := strings.Cut(f, "\t")
			if _, err := feedURL(u); err != nil || kept >= feedsPerPage {
				continue
			}
			add = append(add, link+"\t"+u+"\t"+typ)
			kept++
		}
		if kept == 0 {
			add = append(add, link+"\t-\t")
		}
	}
	if len(add) == 0 {
		return nil
	}
	return writeFile(path, []byte(string(old)+join(add)))
}

// digest sends the items that came in since the last digest, once a day.
// The first run after the opt-in sends nothing: it only notes the items of
// that day, so that nobody gets the whole history.
func (s *server) digest(a Account, dir string, feeds []Feed) error {
	mailedPath, digestPath := filepath.Join(dir, "mailed"), filepath.Join(dir, "digest")
	now := s.feeds.now()
	stamp := []byte(strconv.FormatInt(now.Unix(), 10) + "\n")
	type feedItems struct {
		f     Feed
		items []Item
	}
	var all []feedItems
	var keys []string
	for _, f := range feeds {
		b, err := os.ReadFile(filepath.Join(dir, "items", f.File))
		if err != nil {
			continue
		}
		items := parseItems(string(b), f)
		all = append(all, feedItems{f, items})
		for _, it := range items {
			keys = append(keys, it.Key)
		}
	}
	old, err := os.ReadFile(mailedPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err := writeFile(mailedPath, []byte(join(keys))); err != nil {
			return err
		}
		return writeFile(digestPath, stamp)
	} else if err != nil {
		return err
	}
	b, _ := os.ReadFile(digestPath)
	last, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if now.Sub(time.Unix(last, 0)) < digestEvery {
		return nil
	}
	mailed := set(lines(string(old)))
	tmp, err := os.MkdirTemp("", "sbm-digest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var files, newKeys []string
	for _, fi := range all {
		var ls []string
		for _, it := range fi.items {
			if !mailed[it.Key] {
				ls = append(ls, it.Raw)
				newKeys = append(newKeys, it.Key)
			}
		}
		if len(ls) == 0 {
			continue
		}
		path := filepath.Join(tmp, fi.f.File)
		if err := os.WriteFile(path, []byte(join(ls)), 0o600); err != nil {
			return err
		}
		files = append(files, path)
	}
	if len(newKeys) == 0 {
		return nil
	}
	body, err := s.feeds.plain(files)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("sbm feed: %d new items", len(newKeys))
	if len(newKeys) == 1 {
		subject = "sbm feed: 1 new item"
	}
	body = strings.TrimRight(body, "\n") + "\n\nYour feed: " + s.site + "/feed\n" +
		"To stop these emails, turn off the daily digest in the settings of that page.\n"
	if err := s.mail(a.Email, subject, body); err != nil {
		return err
	}
	if err := writeFile(mailedPath, []byte(string(old)+join(newKeys))); err != nil {
		return err
	}
	return writeFile(digestPath, stamp)
}
