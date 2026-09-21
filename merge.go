package main

import (
	"regexp"
	"strings"
)

var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// norm gives the form of a URL that bm uses to find duplicates: two URLs are
// the same bookmark when they differ only in scheme, a leading "www.",
// trailing slashes or the case of the host.
func norm(u string) string {
	u = scheme.ReplaceAllString(u, "")
	host, rest := u, ""
	if i := strings.IndexByte(u, '/'); i >= 0 {
		host, rest = u[:i], u[i:]
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	return host + strings.TrimRight(rest, "/")
}

// key gives the bookmark that a line holds: the normal form of its URL, the
// first field. Comments and empty lines hold no bookmark and give "".
func key(line string) string {
	if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	u, _, _ := strings.Cut(line, "\t")
	return norm(u)
}

// lines splits a file into lines without their ends. CRLF counts as LF.
func lines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// join is the reverse of lines: each line ends with LF.
func join(ls []string) string {
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, "\n") + "\n"
}

func set(ls []string) map[string]bool {
	m := make(map[string]bool, len(ls))
	for _, l := range ls {
		m[l] = true
	}
	return m
}

// merge combines two versions of a bookmark file that both come from base:
// local, which a device sends, and remote, which the server holds.
//
// The result is remote with the changes that local made since base. A line
// that local removed goes. A line that local added comes in: at the end,
// where bm adds bookmarks, or in the place of the remote line for the same
// bookmark, so that an edit on the device wins. Lines are compared whole.
// When base is unknown, pass "": nothing is removed, and the result is the
// union of both files.
func merge(base, local, remote string) string {
	b := set(lines(base))
	l := lines(local)
	inLocal := set(l)
	r := lines(remote)
	inRemote := set(r)

	var added []string
	first := map[string]int{} // key of an added line: its index in added
	for _, x := range l {
		if b[x] {
			continue
		}
		if k := key(x); k != "" {
			if _, ok := first[k]; !ok {
				first[k] = len(added)
			}
		}
		added = append(added, x)
	}
	used := make([]bool, len(added))

	// replacement gives the added line that takes the place of x, if any.
	replacement := func(x string) (string, bool) {
		k := key(x)
		if k == "" {
			return "", false
		}
		i, ok := first[k]
		if !ok || used[i] || inRemote[added[i]] {
			return "", false
		}
		used[i] = true
		return added[i], true
	}

	var out []string
	for _, x := range r {
		if inLocal[x] {
			out = append(out, x)
			continue
		}
		// Local does not have x: local removed or changed it since base, or
		// x is new on the server.
		if y, ok := replacement(x); ok {
			out = append(out, y)
		} else if !b[x] {
			out = append(out, x)
		}
	}
	for i, y := range added {
		if !used[i] && !inRemote[y] {
			out = append(out, y)
		}
	}
	return join(out)
}
