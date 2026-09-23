package main

// The web pages for the bookmarks of an account. Each change goes through
// Store.sync, as the file of a device does, so the changes that other
// devices made since a page opened stay.

import (
	_ "embed"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxRows   = 1000     // bookmarks on one page
	maxForm   = 64 << 10 // bytes in the form of one bookmark
	maxImport = 32 << 20 // bytes in an imported file: browsers put icons in it
)

var (
	errNoURL   = errors.New("type the URL of the bookmark")
	errHashURL = errors.New("a URL cannot start with #, because bm reads the line as a comment")
	// errChanged tells that a page shows a line that the file does not have
	// now.
	errChanged = errors.New("the bookmark changed on another device")
)

// searchJS is the live search of the bookmark pages.
//
//go:embed search.js
var searchJS string

// previewJS is the live preview of the list page: the page of the
// highlighted bookmark in a frame.
//
//go:embed preview.js
var previewJS string

// sentence gives the text of an error as a sentence for a page.
func sentence(err error) string {
	return strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."
}

// ---- the bookmark file ----

// Bookmark is one bookmark line of the file:
//
//	URL<tab>description<tab>tag tag tag
type Bookmark struct {
	URL, Desc string
	Tags      []string
	N         int // the index of the line in the file
}

// Title gives the text of the link to the bookmark.
func (b Bookmark) Title() string {
	if strings.TrimSpace(b.Desc) == "" {
		return b.URL
	}
	return b.Desc
}

// Host gives the host of the URL, to show under the title.
func (b Bookmark) Host() string {
	u := scheme.ReplaceAllString(b.URL, "")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// Link gives the address of the link to the bookmark. Only http and https
// links are made, so that a line of the file cannot run script in the page.
// An address without a scheme gets https. Other schemes give "": the page
// shows them as text.
func (b Bookmark) Link() string {
	l := strings.ToLower(b.URL)
	switch {
	case strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://"):
		return b.URL
	case !strings.Contains(b.URL, ":"):
		return "https://" + b.URL
	}
	return ""
}

// parseLine gives the bookmark of a line, and false for a comment or an
// empty line. A line without a tab has the old format "URL description |
// tags". Until bm-migrate converts it, its first word is the URL, as in the
// add-on.
func parseLine(line string) (Bookmark, bool) {
	if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
		return Bookmark{}, false
	}
	fields := strings.Split(line, "\t")
	if len(fields) == 1 {
		words := strings.Fields(line)
		return Bookmark{URL: words[0], Desc: strings.Join(words[1:], " ")}, true
	}
	b := Bookmark{URL: strings.TrimSpace(fields[0]), Desc: fields[1]}
	if len(fields) > 2 {
		b.Tags = strings.FieldsFunc(fields[2], func(r rune) bool { return r == ' ' })
	}
	return b, b.URL != ""
}

// parse gives the bookmarks of a file, in the order of the file.
func parse(text string) []Bookmark {
	var out []Bookmark
	for i, l := range lines(text) {
		if b, ok := parseLine(l); ok {
			b.N = i
			out = append(out, b)
		}
	}
	return out
}

// entry is the text in the fields of a bookmark form.
type entry struct{ URL, Desc, Tags string }

// line gives the line of the bookmark, as bm and the add-on write it: the URL
// without white space, the description without tabs and line breaks, and the
// tags with one space between them.
func (e entry) line() (string, error) {
	u := strings.Join(strings.Fields(e.URL), "")
	switch {
	case u == "":
		return "", errNoURL
	case strings.HasPrefix(u, "#"):
		return "", errHashURL
	}
	desc := strings.TrimSpace(strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(e.Desc))
	return u + "\t" + desc + "\t" + strings.Join(strings.Fields(e.Tags), " "), nil
}

// ---- search ----

// score gives the score of query in text, higher for a better match, or -1
// when a word of the query does not occur in text. This is the search of the
// add-on (lib/fuzzy.js), as in fzf: each word of the query must occur in the
// text, with its letters in order but not always together. Case does not
// matter.
func score(query, text string) int {
	hay := strings.ToLower(text)
	total := 0
	for _, word := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return r == ' ' || r == '\t' }) {
		s := scoreWord(word, hay)
		if s < 0 {
			return -1
		}
		total += s
	}
	return total
}

// scoreWord gives the score of one word in hay. The whole word in one place
// scores highest, most at the start of a word.
func scoreWord(word, hay string) int {
	if at := strings.Index(hay, word); at >= 0 {
		s := 100 + utf8.RuneCountInString(word)
		if startsWord(hay, at) {
			s += 20
		}
		return s
	}
	s, end := 0, -1 // end: the byte after the last letter found
	for _, c := range word {
		from := max(end, 0)
		i := strings.IndexRune(hay[from:], c)
		if i < 0 {
			return -1
		}
		i += from
		s++
		if i == end {
			s += 5
		}
		if startsWord(hay, i) {
			s += 3
		}
		_, n := utf8.DecodeRuneInString(hay[i:])
		end = i + n
	}
	return s
}

// startsWord tells if the letter at byte i of hay starts a word: no letter
// or digit is before it.
func startsWord(hay string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(hay[:i])
	return !unicode.IsLetter(r) && !unicode.IsNumber(r)
}

// search gives the bookmarks that match query, best first. Bookmarks with the
// same score keep their order.
func search(bs []Bookmark, query string) []Bookmark {
	type hit struct {
		b     Bookmark
		score int
	}
	var hits []hit
	for _, b := range bs {
		if s := score(query, b.Desc+" "+strings.Join(b.Tags, " ")+" "+b.URL); s >= 0 {
			hits = append(hits, hit{b, s})
		}
	}
	slices.SortStableFunc(hits, func(x, y hit) int { return y.score - x.score })
	out := make([]Bookmark, len(hits))
	for i, h := range hits {
		out[i] = h.b
	}
	return out
}

// orders are the sort orders of the list page. bm also has "used", but the
// counts of use stay on each device.
var orders = map[string]func(x, y Bookmark) int{
	"recent": func(x, y Bookmark) int { return y.N - x.N },
	"url":    func(x, y Bookmark) int { return strings.Compare(norm(x.URL), norm(y.URL)) },
	"desc": func(x, y Bookmark) int {
		return strings.Compare(strings.ToLower(x.Title()), strings.ToLower(y.Title()))
	},
	"tag": func(x, y Bookmark) int { return strings.Compare(strings.Join(x.Tags, " "), strings.Join(y.Tags, " ")) },
}

// target gives the address for text that no bookmark matches, as bm does: one
// word with "://" or a dot is an address, and other text is a web search.
func target(text string) string {
	t := strings.TrimSpace(text)
	if !strings.Contains(t, " ") {
		if strings.Contains(t, "://") {
			return t
		}
		if strings.Contains(t, ".") {
			return "https://" + t
		}
	}
	return "https://duckduckgo.com/?q=" + url.QueryEscape(t)
}

// ---- import ----

var (
	notTag   = regexp.MustCompile(`[^a-z0-9]+`)
	imported = regexp.MustCompile(`^(https?|ftp|file)://`)
)

// importRows gives the bookmark lines of an imported file: the Netscape HTML
// that browsers export, or a file of bookmark lines.
func importRows(text string) []string {
	text = strings.TrimPrefix(text, string(rune(0xFEFF)))
	if strings.HasPrefix(strings.TrimSpace(text), "<") {
		return fromHTML(text)
	}
	return lines(text)
}

// fromHTML gives the bookmark lines of the Netscape HTML that browsers
// export, as bm-import does: <H3> opens a folder, </DL> closes it, and the
// folders of a bookmark become its tags. Only http, https, ftp and file URLs
// stay.
func fromHTML(text string) []string {
	var folders, out []string
	for _, line := range lines(text) {
		upper := upperASCII(line)
		if at := strings.Index(upper, "<H3"); at >= 0 {
			name := line[at:]
			name = name[strings.IndexByte(name, '>')+1:]
			if end := strings.Index(upperASCII(name), "</H3"); end >= 0 {
				name = name[:end]
			}
			folders = append(folders, folderTag(htmlText(name)))
		}
		if at := strings.Index(upper, `HREF="`); at >= 0 {
			u, rest, ok := strings.Cut(line[at+len(`HREF="`):], `"`)
			name := rest[strings.IndexByte(rest, '>')+1:]
			if end := strings.Index(upperASCII(name), "</A"); end >= 0 {
				name = name[:end]
			}
			var tags []string
			for _, f := range folders {
				if f != "" && !slices.Contains(tags, f) {
					tags = append(tags, f)
				}
			}
			if u = htmlText(u); ok && imported.MatchString(u) {
				out = append(out, u+"\t"+htmlText(name)+"\t"+strings.Join(tags, " "))
			}
		}
		if strings.Contains(upper, "</DL>") && len(folders) > 0 {
			folders = folders[:len(folders)-1]
		}
	}
	return out
}

// upperASCII gives s with the letters a to z in upper case. Each byte keeps
// its index, so an index in the result is an index in s.
func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// htmlText gives the text of HTML without its entities, with one space in
// place of each run of white space.
func htmlText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// folderTag gives the tag of a folder name, as bm-import makes it: "Dev
// Tools" is dev-tools.
func folderTag(name string) string {
	return strings.Trim(notTag.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// mergeRows gives the rows whose URL is not in text and not in a row before
// them, as bm --merge does, with the number of rows that it skipped for that
// reason. Empty rows and comments go.
func mergeRows(text string, rows []string) ([]string, int) {
	seen := map[string]bool{}
	for _, l := range lines(text) {
		seen[key(l)] = true
	}
	var add []string
	skipped := 0
	for _, row := range rows {
		k := key(row)
		switch {
		case k == "":
		case seen[k]:
			skipped++
		default:
			seen[k] = true
			f := append(strings.SplitN(row, "\t", 4), "", "")
			add = append(add, f[0]+"\t"+f[1]+"\t"+f[2])
		}
	}
	return add, skipped
}

// ---- pages ----

// bookmarksPage is the data of the pages for the bookmarks of an account.
type bookmarksPage struct {
	page
	Query, Tag, Sort, Done string
	Open                   string     // the form of the list page to open: "add" or "import"
	Version                string     // the version of the file that the page shows
	Rows                   []Bookmark // the bookmarks to show, at most maxRows
	Found, Total           int        // the bookmarks that match, and all bookmarks
	Tags                   []tagCount
	Target                 string // where the query goes when no bookmark matches
	Form                   entry  // the fields of the form of a bookmark
	N                      int    // the line of the bookmark on the edit page
	Text                   string // the whole file, for the file page
}

type tagCount struct {
	Name string
	N    int
}

// done gives the message of the list page after a change.
var done = map[string]string{
	"added":   "Bookmark added.",
	"saved":   "Bookmark saved.",
	"deleted": "Bookmark deleted.",
	"file":    "File saved.",
	"merged":  "File saved, and merged with the changes of your other devices.",
	"changed": "That bookmark changed on another device. Look at it again.",
}

// owner gives the signed-in account when it can use its bookmarks on this
// site: as for sync, its email is confirmed and sync is not paused. Else
// owner sends the browser to sign in, or to the account page, which tells
// why, and gives nil.
func (s *server) owner(w http.ResponseWriter, r *http.Request) *Account {
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	if a := s.store.view(u); !s.verified(a) || !s.active(a) {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return nil
	}
	return u
}

// listing gives the list page for the bookmarks in text that match the query
// and the tag, in the order.
func (s *server) listing(text, version, query, tag, order string) bookmarksPage {
	p := bookmarksPage{page: s.page("Your bookmarks"), Query: query, Tag: tag, Version: version}
	all := parse(text)
	count := map[string]int{}
	var rows []Bookmark
	for _, b := range all {
		for _, t := range b.Tags {
			count[t]++
		}
		if tag == "" || slices.Contains(b.Tags, tag) {
			rows = append(rows, b)
		}
	}
	for name, n := range count {
		p.Tags = append(p.Tags, tagCount{name, n})
	}
	slices.SortFunc(p.Tags, func(x, y tagCount) int { return strings.Compare(x.Name, y.Name) })
	if orders[order] == nil {
		order = "recent"
	}
	p.Sort = order
	slices.SortStableFunc(rows, orders[order])
	if strings.TrimSpace(query) != "" {
		if rows = search(rows, query); len(rows) == 0 {
			p.Target = target(query)
		}
	}
	p.Total, p.Found = len(all), len(rows)
	p.Rows = rows[:min(len(rows), maxRows)]
	return p
}

// bookmarks shows the bookmarks that match the query ?q= and the tag ?tag=,
// in the order ?sort=.
func (s *server) bookmarks(w http.ResponseWriter, r *http.Request) {
	u := s.owner(w, r)
	if u == nil {
		return
	}
	text, version, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	q := r.URL.Query()
	p := s.listing(text, version, q.Get("q"), q.Get("tag"), q.Get("sort"))
	p.Done = done[q.Get("done")]
	if q.Get("done") == "imported" {
		added, _ := strconv.Atoi(q.Get("added"))
		known, _ := strconv.Atoi(q.Get("known"))
		p.Done = fmt.Sprintf("Bookmarks imported: %d. Skipped because they are bookmarks already: %d.", added, known)
	}
	s.render(w, http.StatusOK, "bookmarks", p)
}

// refuse shows the list page again, with the problem of its add or import
// form.
func (s *server) refuse(w http.ResponseWriter, u *Account, status int, form, problem string, e entry) {
	text, version, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	p := s.listing(text, version, "", "", "")
	p.Error, p.Open, p.Form = problem, form, e
	s.render(w, status, "bookmarks", p)
}

// addBookmark adds a bookmark at the end of the file, where bm adds them. As
// bm does, it refuses a URL that is a bookmark already.
func (s *server) addBookmark(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	e := entry{URL: r.PostFormValue("url"), Desc: r.PostFormValue("desc"), Tags: r.PostFormValue("tags")}
	line, err := e.line()
	if err != nil {
		s.refuse(w, u, http.StatusBadRequest, "add", sentence(err), e)
		return
	}
	text, version, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, b := range parse(text) {
		if norm(b.URL) == key(line) {
			s.refuse(w, u, http.StatusConflict, "add", "This URL is a bookmark already: "+b.Title()+".", e)
			return
		}
	}
	if _, _, err := s.store.sync(u, version, join(append(lines(text), line))); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/bookmarks?done=added", http.StatusSeeOther)
}

// lineAt gives line n of version v of the file of the account, and n as a
// number. It gives errChanged when the version is gone, or when the line is
// not a bookmark.
func (s *server) lineAt(u *Account, v, n string) (string, int, error) {
	i, err := strconv.Atoi(n)
	if err != nil || !hashName.MatchString(v) {
		return "", 0, errChanged
	}
	text, _, err := s.store.read(u, v)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, errChanged
	} else if err != nil {
		return "", 0, err
	}
	ls := lines(text)
	if i < 0 || i >= len(ls) {
		return "", 0, errChanged
	}
	if _, ok := parseLine(ls[i]); !ok {
		return "", 0, errChanged
	}
	return ls[i], i, nil
}

// editBookmark shows the form for line ?n= of version ?v= of the file.
func (s *server) editBookmark(w http.ResponseWriter, r *http.Request) {
	u := s.owner(w, r)
	if u == nil {
		return
	}
	q := r.URL.Query()
	line, n, err := s.lineAt(u, q.Get("v"), q.Get("n"))
	if errors.Is(err, errChanged) {
		http.Redirect(w, r, "/bookmarks?done=changed", http.StatusSeeOther)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	b, _ := parseLine(line)
	s.render(w, http.StatusOK, "edit", bookmarksPage{page: s.page("Edit a bookmark"), Version: q.Get("v"), N: n,
		Form: entry{URL: b.URL, Desc: b.Desc, Tags: strings.Join(b.Tags, " ")}})
}

// saveBookmark changes or deletes the bookmark of the edit page in the
// current file. When the current file does not have the line of the page,
// another device changed it, and nothing changes.
func (s *server) saveBookmark(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	v := r.PostFormValue("v")
	old, n, err := s.lineAt(u, v, r.PostFormValue("n"))
	if errors.Is(err, errChanged) {
		http.Redirect(w, r, "/bookmarks?done=changed", http.StatusSeeOther)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	e := entry{URL: r.PostFormValue("url"), Desc: r.PostFormValue("desc"), Tags: r.PostFormValue("tags")}
	problem := func(status int, text string) {
		p := bookmarksPage{page: s.page("Edit a bookmark"), Version: v, N: n, Form: e}
		p.Error = text
		s.render(w, status, "edit", p)
	}
	line, msg := "", "deleted"
	if r.PostFormValue("do") != "delete" {
		if line, err = e.line(); err != nil {
			problem(http.StatusBadRequest, sentence(err))
			return
		}
		msg = "saved"
	}
	text, version, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	if b, _ := parseLine(old); line != "" && key(line) != norm(b.URL) {
		for _, other := range parse(text) {
			if norm(other.URL) == key(line) {
				problem(http.StatusConflict, "Another bookmark has this URL: "+other.Title()+".")
				return
			}
		}
	}
	var out []string
	found := false
	for _, l := range lines(text) {
		if l != old {
			out = append(out, l)
		} else if !found {
			found = true
			if line != "" {
				out = append(out, line)
			}
		}
	}
	if !found {
		http.Redirect(w, r, "/bookmarks?done=changed", http.StatusSeeOther)
		return
	}
	if _, _, err := s.store.sync(u, version, join(out)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/bookmarks?done="+msg, http.StatusSeeOther)
}

// bookmarkFile shows the whole file in a text box, as bm -e opens it in an
// editor.
func (s *server) bookmarkFile(w http.ResponseWriter, r *http.Request) {
	u := s.owner(w, r)
	if u == nil {
		return
	}
	text, version, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "file", bookmarksPage{page: s.page("Edit the file"), Version: version, Text: text})
}

// saveFile takes the whole file of the file page. As for the file of a
// device, the server merges the changes since the version that the page
// showed.
func (s *server) saveFile(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	// A form sends a tab as %09, so the form can be three times as large as
	// the file.
	r.Body = http.MaxBytesReader(w, r.Body, 3*maxFile+maxForm)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "the bookmark file is too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read the form", http.StatusBadRequest)
		}
		return
	}
	base, text := r.PostFormValue("base"), join(lines(r.PostFormValue("text")))
	if base != "" && !hashName.MatchString(base) {
		http.Error(w, "base is not a version name", http.StatusBadRequest)
		return
	}
	if len(text) > maxFile {
		p := bookmarksPage{page: s.page("Edit the file"), Version: base, Text: text}
		p.Error = "The file is larger than 4 MB. Nothing changed."
		s.render(w, http.StatusRequestEntityTooLarge, "file", p)
		return
	}
	merged, _, err := s.store.sync(u, base, text)
	if err != nil {
		s.fail(w, err)
		return
	}
	msg := "file"
	if merged != text {
		msg = "merged"
	}
	http.Redirect(w, r, "/bookmarks?done="+msg, http.StatusSeeOther)
}

// importFile adds the bookmarks of a file, as bm-import and bm --merge do. A
// URL that is a bookmark already is skipped.
func (s *server) importFile(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(w, r) {
		return
	}
	u := s.owner(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)
	f, _, err := r.FormFile("file")
	if err != nil {
		s.refuse(w, u, http.StatusBadRequest, "import", "Choose a file of 32 MB or less.", entry{})
		return
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		s.fail(w, err)
		return
	}
	text, version, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	add, skipped := mergeRows(text, importRows(string(b)))
	merged := join(append(lines(text), add...))
	if len(merged) > maxFile {
		s.refuse(w, u, http.StatusRequestEntityTooLarge, "import",
			"With these bookmarks, the file would be larger than 4 MB. Nothing changed.", entry{})
		return
	}
	if len(add) > 0 {
		if _, _, err := s.store.sync(u, version, merged); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/bookmarks?done=imported&added=%d&known=%d", len(add), skipped), http.StatusSeeOther)
}

// download gives the current file. It works also when sync is paused,
// because the bookmarks belong to their owner.
func (s *server) download(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	text, _, err := s.store.read(u, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="bookmarks.txt"`)
	io.WriteString(w, text)
}

// script gives the live search of the bookmark pages.
func (s *server) script(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, searchJS)
}

// previewScript gives the live preview of the list page.
func (s *server) previewScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, previewJS)
}

// openSearch describes the search of the bookmarks in the OpenSearch 1.1
// format, so that a browser can search them from its address bar.
func (s *server) openSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
<ShortName>sbm</ShortName>
<Description>Search your sbm bookmarks</Description>
<InputEncoding>UTF-8</InputEncoding>
<Url type="text/html" method="get" template="%s/bookmarks?q={searchTerms}"/>
</OpenSearchDescription>
`, html.EscapeString(s.site))
}
