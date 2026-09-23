package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// get gives the status, the Location header and the body of a page.
func (e *harness) get(c *http.Client, path string) (int, string, string) {
	resp, err := c.Get(e.web.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// send posts a form, and gives the status, the Location header and the body.
func (e *harness) send(c *http.Client, path string, form url.Values) (int, string, string) {
	resp := e.post(c, path, form)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// upload sends a file with the import form, as a browser does.
func (e *harness) upload(c *http.Client, content string) (int, string, string) {
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	f, _ := m.CreateFormFile("file", "bookmarks.html")
	io.WriteString(f, content)
	m.Close()
	req, _ := http.NewRequest("POST", e.web.URL+"/bookmarks/import", &body)
	req.Header.Set("Content-Type", m.FormDataContentType())
	req.Header.Set("Origin", e.web.URL)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

// account makes an account whose file is text. It gives a signed-in browser,
// the token of a device and the version of the file.
func (e *harness) account(text string) (*http.Client, string, string) {
	c := e.signup("me@example.org")
	tok := e.token("me@example.org")
	code, _, v := e.sync(tok, "", text)
	if code != http.StatusOK {
		e.t.Fatalf("first sync: %d", code)
	}
	return c, tok, v
}

// fileOf gives the file on the server, as a device without a file gets it.
func (e *harness) fileOf(tok string) string {
	_, text, _ := e.sync(tok, "", "")
	return text
}

var editLink = regexp.MustCompile(`href="/bookmarks/edit\?v=([0-9a-f]{64})&amp;n=([0-9]+)"`)

// editForm gives the fields v and n of the edit link of the bookmark with the
// title on the page.
func editForm(t *testing.T, page, title string) url.Values {
	t.Helper()
	for _, li := range strings.Split(page, "<li>") {
		if strings.Contains(li, ">"+title+"</a>") {
			if m := editLink.FindStringSubmatch(li); m != nil {
				return url.Values{"v": {m[1]}, "n": {m[2]}}
			}
		}
	}
	t.Fatalf("no edit link for %q on the page:\n%s", title, page)
	return nil
}

var rowTitle = regexp.MustCompile(`<li>(?:<a href="[^"]*"[^>]*>)?([^<]*)`)

// titles gives the titles of the bookmarks on the page, in their order.
func titles(page string) []string {
	var out []string
	for _, m := range rowTitle.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestBookmarkPagesNeedSignIn(t *testing.T) {
	e := newEnv(t, false)
	c := e.browser()
	for _, path := range []string{"/bookmarks", "/bookmarks/file", "/bookmarks.txt",
		"/bookmarks/edit?v=" + strings.Repeat("0", 64) + "&n=0"} {
		if code, to, _ := e.get(c, path); code != http.StatusSeeOther || to != "/login" {
			t.Errorf("GET %s: %d to %q", path, code, to)
		}
	}
	for _, path := range []string{"/bookmarks/add", "/bookmarks/edit", "/bookmarks/file", "/bookmarks/import"} {
		if code, to, _ := e.send(c, path, url.Values{"url": {"https://a.org"}}); code != http.StatusSeeOther || to != "/login" {
			t.Errorf("POST %s: %d to %q", path, code, to)
		}
	}
}

func TestSignInOpensTheBookmarks(t *testing.T) {
	e := newEnv(t, false)
	form := url.Values{"email": {"me@example.org"}, "password": {"password1"}}
	if code, to, _ := e.send(e.browser(), "/signup", form); code != http.StatusSeeOther || to != "/account" {
		t.Errorf("sign up: %d to %q", code, to)
	}
	c := e.browser()
	if code, to, _ := e.send(c, "/login", form); code != http.StatusSeeOther || to != "/bookmarks" {
		t.Errorf("sign in: %d to %q", code, to)
	}
	if code, _, page := e.get(c, "/bookmarks"); code != http.StatusOK || !strings.Contains(page, "0 of 0 bookmarks.") {
		t.Errorf("bookmarks of a new account: %d\n%s", code, page)
	}
	for _, path := range []string{"/", "/account"} {
		if !strings.Contains(e.body(c, path), `<a href="/bookmarks">`) {
			t.Errorf("%s has no link to the bookmarks", path)
		}
	}
}

func TestWebChangesReachTheDevices(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("https://a.org\tA\tx\nhttps://b.org\tB\t\n")
	if got := titles(e.body(c, "/bookmarks")); !slices.Equal(got, []string{"B", "A"}) {
		t.Fatalf("titles: %q", got)
	}
	code, to, _ := e.send(c, "/bookmarks/add", url.Values{"url": {" https://c.org "}, "desc": {"C\tc"}, "tags": {" y  z "}})
	if code != http.StatusSeeOther || to != "/bookmarks?done=added" {
		t.Fatalf("add: %d to %q", code, to)
	}
	if got, want := e.fileOf(tok), "https://a.org\tA\tx\nhttps://b.org\tB\t\nhttps://c.org\tC c\ty z\n"; got != want {
		t.Fatalf("after the add: %q, want %q", got, want)
	}
	if !strings.Contains(e.body(c, to), "Bookmark added.") {
		t.Error("the page does not tell that the bookmark is added")
	}

	form := editForm(t, e.body(c, "/bookmarks"), "A")
	page := e.body(c, "/bookmarks/edit?v="+form.Get("v")+"&n="+form.Get("n"))
	if !strings.Contains(page, `name="url" value="https://a.org"`) || !strings.Contains(page, `name="tags" value="x"`) {
		t.Fatalf("edit page:\n%s", page)
	}
	form.Set("url", "https://a.org")
	form.Set("desc", "A, edited")
	form.Set("tags", "x w")
	if code, to, _ := e.send(c, "/bookmarks/edit", form); code != http.StatusSeeOther || to != "/bookmarks?done=saved" {
		t.Fatalf("save: %d to %q", code, to)
	}
	form = editForm(t, e.body(c, "/bookmarks"), "B")
	form.Set("do", "delete")
	if code, to, _ := e.send(c, "/bookmarks/edit", form); code != http.StatusSeeOther || to != "/bookmarks?done=deleted" {
		t.Fatalf("delete: %d to %q", code, to)
	}
	if got, want := e.fileOf(tok), "https://a.org\tA, edited\tx w\nhttps://c.org\tC c\ty z\n"; got != want {
		t.Fatalf("after the edit and the delete: %q, want %q", got, want)
	}
}

func TestWebEditsKeepTheChangesOfDevices(t *testing.T) {
	e := newEnv(t, false)
	c, tok, v := e.account("https://a.org\tA\t\nhttps://b.org\tB\t\n")
	form := editForm(t, e.body(c, "/bookmarks"), "A")
	// A device adds a bookmark while the page is open.
	e.sync(tok, v, "https://a.org\tA\t\nhttps://b.org\tB\t\nhttps://d.org\tD\t\n")
	form.Set("do", "delete")
	if code, to, _ := e.send(c, "/bookmarks/edit", form); code != http.StatusSeeOther || to != "/bookmarks?done=deleted" {
		t.Fatalf("delete: %d to %q", code, to)
	}
	if got, want := e.fileOf(tok), "https://b.org\tB\t\nhttps://d.org\tD\t\n"; got != want {
		t.Fatalf("after the delete: %q, want %q", got, want)
	}

	// A device edits B while the page is open. The page does not change B.
	form = editForm(t, e.body(c, "/bookmarks"), "B")
	_, text, v := e.sync(tok, "", "")
	e.sync(tok, v, strings.Replace(text, "\tB\t", "\tB on a device\t", 1))
	form.Set("url", "https://b.org")
	form.Set("desc", "B on the web")
	if code, to, _ := e.send(c, "/bookmarks/edit", form); code != http.StatusSeeOther || to != "/bookmarks?done=changed" {
		t.Fatalf("save of a bookmark that a device changed: %d to %q", code, to)
	}
	if got, want := e.fileOf(tok), "https://b.org\tB on a device\t\nhttps://d.org\tD\t\n"; got != want {
		t.Fatalf("after the refused save: %q, want %q", got, want)
	}
}

func TestAddAndEditRefusals(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("http://a.org/\tA\t\nhttps://b.org\tB\t\n")
	file := e.fileOf(tok)
	for _, tc := range []struct {
		form url.Values
		code int
		text string
	}{
		{url.Values{"url": {"https://www.A.org"}, "desc": {"typed"}}, http.StatusConflict, "This URL is a bookmark already: A."},
		{url.Values{"url": {" "}}, http.StatusBadRequest, "Type the URL of the bookmark."},
		{url.Values{"url": {"#x"}}, http.StatusBadRequest, "A URL cannot start with #"},
	} {
		code, _, page := e.send(c, "/bookmarks/add", tc.form)
		if code != tc.code || !strings.Contains(page, tc.text) || !strings.Contains(page, "<details open>") {
			t.Errorf("add %v: %d, want %d with %q in an open form", tc.form, code, tc.code, tc.text)
		}
	}
	if _, _, page := e.send(c, "/bookmarks/add", url.Values{"url": {"https://a.org"}, "desc": {"typed"}}); !strings.Contains(page, `value="typed"`) {
		t.Error("the add form lost the text that was typed")
	}

	form := editForm(t, e.body(c, "/bookmarks"), "A")
	form.Set("url", "https://b.org/")
	if code, _, page := e.send(c, "/bookmarks/edit", form); code != http.StatusConflict || !strings.Contains(page, "Another bookmark has this URL: B.") {
		t.Errorf("edit to the URL of B: %d", code)
	}
	form.Set("url", "")
	if code, _, _ := e.send(c, "/bookmarks/edit", form); code != http.StatusBadRequest {
		t.Errorf("edit without a URL: %d", code)
	}
	// A version that is gone, or a line that the version does not have.
	if code, to, _ := e.get(c, "/bookmarks/edit?v="+strings.Repeat("0", 64)+"&n=0"); code != http.StatusSeeOther || to != "/bookmarks?done=changed" {
		t.Errorf("edit page of an unknown version: %d to %q", code, to)
	}
	if code, to, _ := e.send(c, "/bookmarks/edit", url.Values{"v": {form.Get("v")}, "n": {"9"}, "url": {"https://x.org"}}); code != http.StatusSeeOther || to != "/bookmarks?done=changed" {
		t.Errorf("save of an unknown line: %d to %q", code, to)
	}
	if got := e.fileOf(tok); got != file {
		t.Errorf("a refusal changed the file: %q, want %q", got, file)
	}
}

func TestSearchTagsAndOrders(t *testing.T) {
	e := newEnv(t, false)
	c, _, _ := e.account("https://b.org\tBee\tx\nhttps://c.org\tAnt\ty\nhttps://a.org\tCat\tz w\n# a comment\nhttps://jisho.org\tJisho: 日本語\tja\n")
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/bookmarks", []string{"Jisho: 日本語", "Cat", "Ant", "Bee"}},
		{"/bookmarks?sort=url", []string{"Cat", "Bee", "Ant", "Jisho: 日本語"}},
		{"/bookmarks?sort=desc", []string{"Ant", "Bee", "Cat", "Jisho: 日本語"}},
		{"/bookmarks?sort=tag", []string{"Jisho: 日本語", "Bee", "Ant", "Cat"}},
		{"/bookmarks?sort=nonsense", []string{"Jisho: 日本語", "Cat", "Ant", "Bee"}},
		{"/bookmarks?q=jsh", []string{"Jisho: 日本語"}},
		{"/bookmarks?q=" + url.QueryEscape("日本"), []string{"Jisho: 日本語"}},
		{"/bookmarks?tag=z", []string{"Cat"}},
		{"/bookmarks?tag=z&q=bee", nil},
	} {
		if got := titles(e.body(c, tc.path)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.path, got, tc.want)
		}
	}
	page := e.body(c, "/bookmarks?q=jsh")
	if !strings.Contains(page, "1 of 4 bookmarks.") || !strings.Contains(page, `<a href="/bookmarks?tag=ja">ja</a>&nbsp;(1)`) {
		t.Errorf("the count or the tags are wrong:\n%s", page)
	}
	// Text that no bookmark matches goes to the web, as in bm.
	if page := e.body(c, "/bookmarks?q=posix+sh"); !strings.Contains(page, "No bookmark matches.") || !strings.Contains(page, "https://duckduckgo.com/?q=posix") {
		t.Errorf("no web search for text that no bookmark matches:\n%s", page)
	}
	if page := e.body(c, "/bookmarks?q=example.org/x"); !strings.Contains(page, `Go to <a href="https://example.org/x" class="open">`) {
		t.Errorf("no link for an address that no bookmark matches:\n%s", page)
	}
}

func TestBookmarkPagesEscapeTheFile(t *testing.T) {
	e := newEnv(t, false)
	c, _, _ := e.account("javascript:alert(1)\t<script>alert(2)</script>\t<marquee>\nwww.example.org\tNo scheme\t\n")
	page := e.body(c, "/bookmarks")
	for _, bad := range []string{`href="javascript:`, "<script>", "<marquee>"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page has %q", bad)
		}
	}
	if !strings.Contains(page, "&lt;script&gt;alert(2)&lt;/script&gt;") {
		t.Error("the page does not show the description as text")
	}
	if !strings.Contains(page, `<a href="https://www.example.org" class="open">No scheme</a>`) {
		t.Error("an address without a scheme does not get https")
	}
}

func TestBookmarksWaitForTheEmailCheck(t *testing.T) {
	e := newEnv(t, false)
	e.withMail()
	c := e.signup("me@example.org")
	if code, to, _ := e.get(c, "/bookmarks"); code != http.StatusSeeOther || to != "/account" {
		t.Errorf("bookmarks before the check: %d to %q", code, to)
	}
	if code, to, _ := e.send(c, "/bookmarks/add", url.Values{"url": {"https://a.org"}}); code != http.StatusSeeOther || to != "/account" {
		t.Errorf("add before the check: %d to %q", code, to)
	}
	if strings.Contains(e.body(c, "/account"), `href="/bookmarks"`) {
		t.Error("the account page links to the bookmarks before the check")
	}
	if code := e.open(e.link()); code != http.StatusOK {
		t.Fatalf("the link of the email: %d", code)
	}
	if code, _, _ := e.get(c, "/bookmarks"); code != http.StatusOK {
		t.Errorf("bookmarks after the check: %d", code)
	}
}

func TestBookmarksPauseWithSync(t *testing.T) {
	e := newEnv(t, true)
	c, tok, _ := e.account("https://a.org\tA\t\n")
	e.s.store.byTok(tok).Created = time.Now().Add(-31 * 24 * time.Hour)
	if code, to, _ := e.get(c, "/bookmarks"); code != http.StatusSeeOther || to != "/account" {
		t.Errorf("bookmarks after the trial: %d to %q", code, to)
	}
	// The download works: the bookmarks belong to their owner.
	resp, err := c.Get(e.web.URL + "/bookmarks.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "https://a.org\tA\t\n" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="bookmarks.txt"` {
		t.Errorf("download after the trial: %s %q %q", resp.Status, b, resp.Header.Get("Content-Disposition"))
	}
}

func TestEditTheWholeFile(t *testing.T) {
	e := newEnv(t, false)
	c, tok, v := e.account("https://a.org\tA\t\nhttps://b.org\tB\t\n")
	page := e.body(c, "/bookmarks/file")
	if !strings.Contains(page, `name="base" value="`+v+`"`) || !strings.Contains(page, ">\nhttps://a.org\tA\t\nhttps://b.org\tB\t\n</textarea>") {
		t.Fatalf("file page:\n%s", page)
	}
	// A device adds D while the page is open. The browser sends CRLF, and a
	// file without A and with C.
	e.sync(tok, v, "https://a.org\tA\t\nhttps://b.org\tB\t\nhttps://d.org\tD\t\n")
	code, to, _ := e.send(c, "/bookmarks/file", url.Values{"base": {v}, "text": {"https://b.org\tB\t\r\nhttps://c.org\tC\t\r\n"}})
	if code != http.StatusSeeOther || to != "/bookmarks?done=merged" {
		t.Fatalf("save: %d to %q", code, to)
	}
	want := "https://b.org\tB\t\nhttps://d.org\tD\t\nhttps://c.org\tC\t\n"
	if got := e.fileOf(tok); got != want {
		t.Fatalf("after the save: %q, want %q", got, want)
	}
	// Without changes of other devices, the file is as the page sent it.
	_, _, v = e.sync(tok, "", "")
	if code, to, _ := e.send(c, "/bookmarks/file", url.Values{"base": {v}, "text": {want + "https://e.org\tE\t\n"}}); code != http.StatusSeeOther || to != "/bookmarks?done=file" {
		t.Errorf("save without other changes: %d to %q", code, to)
	}
	if code, _, _ := e.send(c, "/bookmarks/file", url.Values{"base": {"../accounts.json"}, "text": {"x"}}); code != http.StatusBadRequest {
		t.Errorf("a base that is not a version name: %d", code)
	}
}

// browserExport is a file as the browsers export it.
const browserExport = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<META HTTP-EQUIV="Content-Type" CONTENT="text/html; charset=UTF-8">
<TITLE>Bookmarks</TITLE>
<H1>Bookmarks</H1>
<DL><p>
    <DT><A HREF="https://www.a.org/" ADD_DATE="1" ICON="data:image/png;base64,AAAA">A &amp; more</A>
    <DT><H3 ADD_DATE="1">Dev Tools</H3>
    <DL><p>
        <DT><A HREF="https://go.dev/doc/">Go   docs</A>
        <DT><H3>Go &amp; Co</H3>
        <DL><p>
            <DT><A HREF="https://pkg.go.dev/">Packages</A>
            <DT><A HREF="https://go.dev/doc">Go docs again</A>
        </DL><p>
        <DT><A HREF="javascript:alert(1)">Bookmarklet</A>
    </DL><p>
    <DT><A HREF="https://news.example/">News</A>
</DL>
`

func TestImportFiles(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("http://a.org\tA\t\n")
	code, to, _ := e.upload(c, browserExport)
	if code != http.StatusSeeOther || to != "/bookmarks?done=imported&added=3&known=2" {
		t.Fatalf("import of a browser file: %d to %q", code, to)
	}
	want := "http://a.org\tA\t\n" +
		"https://go.dev/doc/\tGo docs\tdev-tools\n" +
		"https://pkg.go.dev/\tPackages\tdev-tools go-co\n" +
		"https://news.example/\tNews\t\n"
	if got := e.fileOf(tok); got != want {
		t.Fatalf("after the import: %q, want %q", got, want)
	}
	if !strings.Contains(e.body(c, to), "Bookmarks imported: 3. Skipped because they are bookmarks already: 2.") {
		t.Error("the page does not tell what the import did")
	}
	// A file of bookmark lines, as bm --merge takes them.
	if code, to, _ := e.upload(c, "https://b.org\tB\tx\n# comment\nhttps://pkg.go.dev\tknown\t\nhttps://c.org\n"); code != http.StatusSeeOther || to != "/bookmarks?done=imported&added=2&known=1" {
		t.Fatalf("import of bookmark lines: %d to %q", code, to)
	}
	if got := e.fileOf(tok); got != want+"https://b.org\tB\tx\nhttps://c.org\t\t\n" {
		t.Fatalf("after the import of lines: %q", got)
	}
	if code, _, page := e.send(c, "/bookmarks/import", url.Values{}); code != http.StatusBadRequest || !strings.Contains(page, "Choose a file") {
		t.Errorf("import without a file: %d", code)
	}
}

func TestOtherSiteCannotChangeBookmarks(t *testing.T) {
	e := newEnv(t, false)
	c, tok, _ := e.account("https://a.org\tA\t\n")
	for _, path := range []string{"/bookmarks/add", "/bookmarks/edit", "/bookmarks/file", "/bookmarks/import"} {
		req, _ := http.NewRequest("POST", e.web.URL+path, strings.NewReader("url=https%3A%2F%2Fevil.example&text="))
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
	if got := e.fileOf(tok); got != "https://a.org\tA\t\n" {
		t.Errorf("another site changed the file: %q", got)
	}
}

func TestSearchFromTheAddressBar(t *testing.T) {
	e := newEnv(t, false)
	resp, err := http.Get(e.web.URL + "/opensearch.xml")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var d struct {
		ShortName string
		URL       struct {
			Type     string `xml:"type,attr"`
			Template string `xml:"template,attr"`
		} `xml:"Url"`
	}
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("the description is not XML: %v\n%s", err, b)
	}
	if resp.Header.Get("Content-Type") != "application/opensearchdescription+xml" || d.ShortName != "sbm" ||
		d.URL.Type != "text/html" || d.URL.Template != e.web.URL+"/bookmarks?q={searchTerms}" {
		t.Errorf("description: %q %+v", resp.Header.Get("Content-Type"), d)
	}
	// Each page tells the browser where the description is.
	if !strings.Contains(e.body(e.browser(), "/"), `<link rel="search" type="application/opensearchdescription+xml" title="sbm bookmarks" href="/opensearch.xml">`) {
		t.Error("the home page does not link to the description")
	}
	// The bookmarks page tells the address for a browser, and that address
	// gives the search.
	c, _, _ := e.account("https://a.org\tA\t\nhttps://b.org\tB\t\n")
	if !strings.Contains(e.body(c, "/bookmarks"), "<code>"+e.web.URL+"/bookmarks?q=%s</code>") {
		t.Error("the bookmarks page does not tell the address for the browser")
	}
	if got := titles(e.body(c, "/bookmarks?q=b.org")); !slices.Equal(got, []string{"B"}) {
		t.Errorf("search from the address bar: %q", got)
	}
}

func TestSearchOnTheHomePage(t *testing.T) {
	e := newEnv(t, false)
	e.withMail()
	if strings.Contains(e.body(e.browser(), "/"), `id="search"`) {
		t.Error("the home page shows the search to a visitor who is not signed in")
	}
	c := e.signup("me@example.org")
	if strings.Contains(e.body(c, "/"), `id="search"`) {
		t.Error("the home page shows the search before the email check")
	}
	e.open(e.link())
	page := e.body(c, "/")
	for _, want := range []string{`<form id="search" method="get" action="/bookmarks">`,
		`<div id="results" data-empty="clear"></div>`, `<script src="/search.js" defer></script>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the home page has no %q", want)
		}
	}
}

func TestLiveSearch(t *testing.T) {
	e := newEnv(t, false)
	c, _, _ := e.account("https://a.org\tA\t\ngemini://b.org\tB\t\n")
	resp, err := c.Get(e.web.URL + "/bookmarks")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Scripts come only from this server, and fetch only from it.
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "connect-src 'self';") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("Content-Security-Policy: %q", csp)
	}
	// The script finds the form, the results, and the links that Enter opens.
	// A bookmark without a web address has no such link.
	for _, want := range []string{`<form id="search" `, `<div id="results">`, `<a href="https://a.org" class="open">A</a>`,
		`<li>B<br>`, `<script src="/search.js" defer></script>`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the page has no %q", want)
		}
	}
	resp, err = e.browser().Get(e.web.URL + "/search.js")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/javascript; charset=utf-8" || string(b) != searchJS {
		t.Errorf("GET /search.js: %s %q, %d bytes", resp.Status, resp.Header.Get("Content-Type"), len(b))
	}
}

func TestLivePreview(t *testing.T) {
	e := newEnv(t, false)
	c, _, _ := e.account("https://a.org\tA\t\n")
	resp, err := c.Get(e.web.URL + "/bookmarks")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The preview shows pages of other sites in a frame, but only https
	// pages: an http frame in an https page is mixed content. No site may
	// show this page in a frame.
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "; frame-src https:;") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy: %q", csp)
	}
	// The frame runs the scripts of the page, but the page cannot lead this
	// page away, open windows or send forms, and it gets no referrer. The
	// script shows the pane.
	for _, want := range []string{`<section id="preview" hidden>`,
		`<iframe title="Preview of the page" sandbox="allow-scripts allow-same-origin" referrerpolicy="no-referrer"></iframe>`,
		`<script src="/preview.js" defer></script>`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the page has no %q", want)
		}
	}
	resp, err = e.browser().Get(e.web.URL + "/preview.js")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/javascript; charset=utf-8" || string(b) != previewJS {
		t.Errorf("GET /preview.js: %s %q, %d bytes", resp.Status, resp.Header.Get("Content-Type"), len(b))
	}
	// The home page has the search, but no room for a preview.
	if home := e.body(c, "/"); !strings.Contains(home, `id="search"`) ||
		strings.Contains(home, `<section id="preview"`) || strings.Contains(home, "/preview.js") {
		t.Error("the home page must have the search and no preview")
	}
}
