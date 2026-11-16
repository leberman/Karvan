package paths

import (
	"path"
	"strings"
)

// The functions below work on paths of a given target OS, independent of the
// host, so that Linux tests can exercise Windows rules and vice versa.
// Windows paths may use "\" or "/"; results always use "\".

func isWindows(goos string) bool { return goos == "windows" }

// Join joins path elements for goos and cleans the result.
func Join(goos string, elem ...string) string {
	if !isWindows(goos) {
		return path.Join(elem...)
	}
	var parts []string
	for _, e := range elem {
		if e != "" {
			parts = append(parts, e)
		}
	}
	return Clean(goos, strings.Join(parts, `\`))
}

// IsAbs reports whether p is absolute on goos. On Windows that means a drive
// letter followed by a separator ("C:\x") or a UNC path ("\\server\share").
func IsAbs(goos, p string) bool {
	if !isWindows(goos) {
		return strings.HasPrefix(p, "/")
	}
	p = strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && p[2] == '\\'
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Clean returns the shortest equivalent of p for goos: no repeated or trailing
// separators, "." and ".." resolved lexically.
func Clean(goos, p string) string {
	if !isWindows(goos) {
		return path.Clean(p)
	}
	p = strings.ReplaceAll(p, "/", `\`)
	prefix := ""
	switch {
	case strings.HasPrefix(p, `\\`): // UNC: keep the leading "\\"
		prefix, p = `\\`, p[2:]
	case len(p) >= 2 && isLetter(p[0]) && p[1] == ':':
		prefix, p = strings.ToUpper(p[:1])+":", p[2:]
	}
	rooted := strings.HasPrefix(p, `\`)
	// Reuse path.Clean on a slash version of the rest.
	c := path.Clean(strings.ReplaceAll(p, `\`, "/"))
	if c == "." && prefix != "" && rooted {
		c = "/"
	}
	return prefix + strings.ReplaceAll(c, "/", `\`)
}

// HasPathPrefix reports whether p is prefix itself or lies inside it, comparing
// whole path components: "/a/app" contains "/a/app/x" but not "/a/app-old"
// (PRD 5). On Windows the comparison ignores letter case and accepts either
// separator. Both paths should be absolute.
func HasPathPrefix(goos, p, prefix string) bool {
	p, prefix = Clean(goos, p), Clean(goos, prefix)
	sep := "/"
	if isWindows(goos) {
		sep = `\`
		p, prefix = strings.ToLower(p), strings.ToLower(prefix)
	}
	if p == prefix {
		return true
	}
	if !strings.HasSuffix(prefix, sep) { // the root "/" or "C:\" already ends with one
		prefix += sep
	}
	return strings.HasPrefix(p, prefix)
}
