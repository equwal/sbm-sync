package main

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestParseLine(t *testing.T) {
	for line, want := range map[string]Bookmark{
		"https://a.org\tA b\tx  y":        {URL: "https://a.org", Desc: "A b", Tags: []string{"x", "y"}},
		" https://a.org \t\t":             {URL: "https://a.org"},
		"https://a.org old format | tags": {URL: "https://a.org", Desc: "old format | tags"},
		"https://a.org":                   {URL: "https://a.org"},
	} {
		got, ok := parseLine(line)
		if !ok || got.URL != want.URL || got.Desc != want.Desc || !slices.Equal(got.Tags, want.Tags) {
			t.Errorf("parseLine(%q) = %+v %v, want %+v", line, got, ok, want)
		}
	}
	for _, line := range []string{"", "  ", "# comment", "\tno URL\t"} {
		if b, ok := parseLine(line); ok {
			t.Errorf("parseLine(%q) = %+v, want no bookmark", line, b)
		}
	}
}

// A form gives one line, which holds the URL without white space, the words
// of the description and the tags.
func TestFormGivesOneLine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := entry{rapid.String().Draw(t, "url"), rapid.String().Draw(t, "desc"), rapid.String().Draw(t, "tags")}
		u := strings.Join(strings.Fields(e.URL), "")
		line, err := e.line()
		if u == "" || strings.HasPrefix(u, "#") {
			if err == nil {
				t.Fatalf("line of URL %q = %q, want an error", e.URL, line)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(line, "\t") != 2 || strings.ContainsAny(line, "\r\n") {
			t.Fatalf("%q is not one line of three fields", line)
		}
		b, ok := parseLine(line)
		if !ok || b.URL != u || !slices.Equal(strings.Fields(b.Desc), strings.Fields(e.Desc)) ||
			!slices.Equal(b.Tags, strings.Fields(e.Tags)) {
			t.Fatalf("parseLine(%q) = %+v %v", line, b, ok)
		}
	})
}

func TestLink(t *testing.T) {
	for u, want := range map[string]string{
		"https://a.org":       "https://a.org",
		"HTTP://A.org/x":      "HTTP://A.org/x",
		"www.a.org/x":         "https://www.a.org/x",
		"javascript:alert(1)": "",
		"JavaScript:alert(1)": "",
		"data:text/html,x":    "",
		"file:///etc/passwd":  "",
	} {
		if got := (Bookmark{URL: u}).Link(); got != want {
			t.Errorf("link of %q = %q, want %q", u, got, want)
		}
	}
}

// TestScoreAsTheAddOn compares with the scores of lib/fuzzy.js in the add-on.
func TestScoreAsTheAddOn(t *testing.T) {
	for _, c := range []struct {
		query, text string
		want        int
	}{
		{"suck", "suckless software c tools https://suckless.org", 124},
		{"sls", "suckless", 6},
		{"jsh", "Jisho: 日本語の辞書 ja https://jisho.org", 11},
		{"日本", "Jisho: 日本語", 122},
		{"go doc", "Go docs dev-tools https://go.dev/doc/", 245},
		{"xyz", "abc", -1},
		{"", "anything", 0},
		{"GITHUB", "https://github.com/equwal/sbm", 126},
		{"eqsbm", "https://github.com/equwal/sbm", 26},
		{"a b", "b a", 242},
		{"tools c", "suckless software c tools", 226},
		{"wiki", "Wikipedia, the free encyclopedia  https://en.wikipedia.org", 124},
		{"wkp", "Wikipedia, the free encyclopedia  https://en.wikipedia.org", 6},
		{"é", "Café crème", 101},
		{"cr", "Café crème", 122},
	} {
		if got := score(c.query, c.text); got != c.want {
			t.Errorf("score(%q, %q) = %d, want %d", c.query, c.text, got, c.want)
		}
	}
}

// letters draws text from few letters, with case, white space, punctuation
// and letters of more than one byte, so that searches often match.
var letters = rapid.StringOf(rapid.SampledFrom([]rune("abcABC xyz.-/:日本éÉ\t")))

// matches is a slow reference for the search: case aside, the letters of
// each word of the query occur in the text in their order.
func matches(query, text string) bool {
	hay := []rune(strings.ToLower(text))
	for _, word := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return r == ' ' || r == '\t' }) {
		w, i := []rune(word), 0
		for _, r := range hay {
			if i < len(w) && r == w[i] {
				i++
			}
		}
		if i < len(w) {
			return false
		}
	}
	return true
}

func TestScoreMatchesAsTheReference(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		query, text := letters.Draw(t, "query"), letters.Draw(t, "text")
		if s := score(query, text); (s >= 0) != matches(query, text) || s < -1 {
			t.Fatalf("score(%q, %q) = %d, reference match %v", query, text, s, matches(query, text))
		}
	})
}

func TestScoreFindsEachPartOfTheText(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := letters.Draw(t, "text")
		rs := []rune(strings.ToLower(text))
		var words []string
		for range rapid.IntRange(1, 3).Draw(t, "words") {
			i := rapid.IntRange(0, len(rs)).Draw(t, "i")
			j := rapid.IntRange(i, len(rs)).Draw(t, "j")
			words = append(words, string(rs[i:j]))
		}
		query := strings.Join(words, " ")
		n := len(strings.FieldsFunc(query, func(r rune) bool { return r == ' ' || r == '\t' }))
		if s := score(query, text); s < 100*n {
			t.Fatalf("score(%q, %q) = %d, want at least %d", query, text, s, 100*n)
		}
	})
}

func TestScoreIgnoresCaseAndWordOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a, b, text := letters.Draw(t, "a"), letters.Draw(t, "b"), letters.Draw(t, "text")
		s := score(a+" "+b, text)
		if got := score(strings.ToUpper(b+" "+a), text); got != s {
			t.Fatalf("score(%q) = %d, but score(%q) = %d", strings.ToUpper(b+" "+a), got, a+" "+b, s)
		}
		if got := score(a+" "+b, strings.ToUpper(text)); got != s {
			t.Fatalf("in %q: %d, in %q: %d", strings.ToUpper(text), got, text, s)
		}
	})
}

func FuzzScore(f *testing.F) {
	f.Add("sls", "suckless")
	f.Add("é", "Café")
	f.Add(string([]byte{0xff}), "a"+string([]byte{0xff, 0xe6, 0x97})+"b")
	f.Fuzz(func(t *testing.T, query, text string) {
		if s := score(query, text); s < -1 {
			t.Fatalf("score(%q, %q) = %d", query, text, s)
		}
	})
}

func TestSearchKeepsTheOrderOfEqualScores(t *testing.T) {
	bs := parse("https://a.org\tgo\t\nhttps://b.org\tgolang\t\nhttps://c.org\tgo\t\nhttps://d.org\tpython\t\n")
	var got []string
	for _, b := range search(bs, "go") {
		got = append(got, b.URL)
	}
	// "go" at the start of a word scores the same in each; b.org has it too.
	if want := []string{"https://a.org", "https://b.org", "https://c.org"}; !slices.Equal(got, want) {
		t.Errorf("search: %q, want %q", got, want)
	}
}

func TestTarget(t *testing.T) {
	for text, want := range map[string]string{
		"suckless.org/dwm": "https://suckless.org/dwm",
		"gemini://x.org":   "gemini://x.org",
		"posix sh printf":  "https://duckduckgo.com/?q=posix+sh+printf",
		" word ":           "https://duckduckgo.com/?q=word",
		"c++":              "https://duckduckgo.com/?q=c%2B%2B",
	} {
		if got := target(text); got != want {
			t.Errorf("target(%q) = %q, want %q", text, got, want)
		}
	}
}

// Each imported row with a new URL comes in once. The other rows are known,
// or are no bookmark.
func TestMergeRowsAddsEachURLOnce(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text, rows := file(t, "file"), lines(file(t, "rows"))
		add, skipped := mergeRows(text, rows)
		seen := map[string]bool{}
		for _, l := range lines(text) {
			seen[key(l)] = true
		}
		for _, a := range add {
			if k := key(a); k == "" || seen[k] {
				t.Fatalf("added %q, but its URL is known", a)
			}
			seen[key(a)] = true
		}
		empty := 0
		for _, r := range rows {
			if key(r) == "" {
				empty++
			}
		}
		if len(add)+skipped+empty != len(rows) {
			t.Fatalf("%d added, %d skipped and %d empty of %d rows", len(add), skipped, empty, len(rows))
		}
	})
}

func TestFolderTag(t *testing.T) {
	for name, want := range map[string]string{"Dev Tools": "dev-tools", " -C++ & Go- ": "c-go", "日本語": ""} {
		if got := folderTag(name); got != want {
			t.Errorf("folderTag(%q) = %q, want %q", name, got, want)
		}
	}
}
