package main

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// file draws a bookmark file from few URLs and descriptions, so that files
// share lines, and lines share bookmarks, as often as real files do.
func file(t *rapid.T, label string) string {
	pool := []string{
		"https://suckless.org/\tsuckless\ttools",
		"https://suckless.org\tsuckless software\ttools c",
		"http://www.suckless.org\tsuckless",
		"https://example.org/a\tA\t",
		"https://example.org/a\tA, edited\tx",
		"https://example.org/b\tB\t",
		"https://jisho.org\tJisho: 日本語の辞書\tja",
		"https://jisho.org\tJisho\t",
		"# comment",
		"",
	}
	ls := rapid.SliceOfN(rapid.SampledFrom(pool), 0, 8).Draw(t, label)
	return join(ls)
}

// unique draws a file without two equal lines.
func unique(t *rapid.T, label string) string {
	ls := lines(file(t, label))
	var out []string
	for _, l := range ls {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return join(out)
}

func sorted(text string) []string {
	ls := lines(text)
	slices.Sort(ls)
	return ls
}

func TestNoLocalChangeGivesRemote(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base, remote := file(t, "base"), file(t, "remote")
		if got := merge(base, base, remote); got != join(lines(remote)) {
			t.Fatalf("merge(base, base, remote) = %q, want %q", got, remote)
		}
	})
}

func TestNoRemoteChangeGivesLocal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base, local := unique(t, "base"), unique(t, "local")
		if got := sorted(merge(base, local, base)); !slices.Equal(got, sorted(local)) {
			t.Fatalf("merge(base, local, base) has lines %q, want %q", got, sorted(local))
		}
	})
}

func TestMergeOfOneFileIsThatFile(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := file(t, "f")
		if got := merge(f, f, f); got != join(lines(f)) {
			t.Fatalf("merge(f, f, f) = %q", got)
		}
	})
}

func TestLocalAdditionsStay(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base, local, remote := file(t, "base"), file(t, "local"), file(t, "remote")
		got := set(lines(merge(base, local, remote)))
		b := set(lines(base))
		for _, l := range lines(local) {
			if !b[l] && !got[l] {
				t.Fatalf("line %q that local added is lost", l)
			}
		}
	})
}

func TestLocalRemovalsStay(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base, local, remote := file(t, "base"), file(t, "local"), file(t, "remote")
		got := set(lines(merge(base, local, remote)))
		l := set(lines(local))
		for _, x := range lines(base) {
			if !l[x] && got[x] {
				t.Fatalf("line %q that local removed came back", x)
			}
		}
	})
}

func TestRemoteAdditionsStayWithoutConflict(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base, local, remote := file(t, "base"), file(t, "local"), file(t, "remote")
		got := set(lines(merge(base, local, remote)))
		b := set(lines(base))
		changed := map[string]bool{} // bookmarks that local added or edited
		for _, l := range lines(local) {
			if !b[l] {
				changed[key(l)] = true
			}
		}
		for _, x := range lines(remote) {
			if !b[x] && !changed[key(x)] && !got[x] {
				t.Fatalf("line %q that the server added is lost", x)
			}
		}
	})
}

func TestNoNewLines(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base, local, remote := file(t, "base"), file(t, "local"), file(t, "remote")
		have := set(append(lines(local), lines(remote)...))
		for _, x := range lines(merge(base, local, remote)) {
			if !have[x] {
				t.Fatalf("line %q comes from neither side", x)
			}
		}
	})
}

// Two devices that sync in turn end with the same file.
func TestDevicesConverge(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		start := file(t, "start")
		a, b := file(t, "device a"), file(t, "device b")
		m1 := merge(start, a, start) // a syncs: a now has m1
		m2 := merge(start, b, m1)    // b syncs: b now has m2
		m3 := merge(m1, m1, m2)      // a syncs again
		if m3 != m2 {
			t.Fatalf("device a has %q, device b has %q", m3, m2)
		}
	})
}

func TestNorm(t *testing.T) {
	same := []string{"https://suckless.org/", "http://suckless.org", "suckless.org//", "https://WWW.Suckless.ORG"}
	for _, u := range same {
		if norm(u) != "suckless.org" {
			t.Errorf("norm(%q) = %q, want suckless.org", u, norm(u))
		}
	}
	if norm("https://example.org/A") == norm("https://example.org/a") {
		t.Error("the case of the path must count")
	}
}

func TestMergeExamples(t *testing.T) {
	cases := []struct{ name, base, local, remote, want string }{
		{"first device", "", "a\t1\n", "", "a\t1\n"},
		{"second device: union", "", "b\t2\n", "a\t1\n", "a\t1\nb\t2\n"},
		{"local add goes to the end", "a\t1\n", "a\t1\nc\t3\n", "a\t1\nb\t2\n", "a\t1\nb\t2\nc\t3\n"},
		{"local delete", "a\t1\nb\t2\n", "b\t2\n", "a\t1\nb\t2\nc\t3\n", "b\t2\nc\t3\n"},
		{"local edit keeps its place", "a\t1\nb\t2\n", "a\tone\nb\t2\n", "a\t1\nb\t2\n", "a\tone\nb\t2\n"},
		{"both edit: the device wins", "a\t1\n", "a\tlocal\n", "a\tremote\n", "a\tlocal\n"},
		{"local delete, remote edit: the edit stays", "a\t1\n", "", "a\tremote\n", "a\tremote\n"},
		{"CRLF", "", "a\t1\r\nb\t2\r\n", "", "a\t1\nb\t2\n"},
		{"no final newline", "", "a\t1", "", "a\t1\n"},
		{"a file of one empty line", "", "\n", "", "\n"},
		{"an empty line from the server", "a\n", "", "a\n\n", "\n"},
		{"same page, other form", "", "https://www.x.org/\tx\n", "http://x.org\tx\n", "https://www.x.org/\tx\n"},
	}
	for _, c := range cases {
		if got := merge(c.base, c.local, c.remote); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestKey(t *testing.T) {
	for _, l := range []string{"", "   ", "# https://x.org"} {
		if key(l) != "" {
			t.Errorf("key(%q) = %q, want none", l, key(l))
		}
	}
	if k := key("https://x.org/a/\tdesc\ttag"); k != "x.org/a" {
		t.Errorf("key = %q", k)
	}
	if !strings.Contains(key("https://jisho.org/word/日本"), "日本") {
		t.Error("key loses non-ASCII text")
	}
}
