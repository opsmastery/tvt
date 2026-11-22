// Package safe checks names that reach the filesystem or a git command
// line from a config or a committed manifest: a repo path must stay
// inside the repo, and a git ref must not read as a flag.
package safe

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RepoPath reports whether p (slash-separated, relative to a repo root)
// names a file inside the repo: not empty, not absolute, no "..".
// tvt joins these paths to checkouts and temporary copies, so one that
// escapes would read or write outside them.
func RepoPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsRune(p, '\\') || strings.ContainsRune(p, 0) {
		return fmt.Errorf("path %q: use forward slashes", p)
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q isn't inside the repo (no absolute paths or ..)", p)
	}
	return nil
}

// Ref reports whether s is usable as a git branch or ref on a command
// line: it can't start with "-" (git would read it as an option, and
// some options run commands), and it follows git check-ref-format's
// character rules.
func Ref(s string) error {
	if s == "" {
		return fmt.Errorf("empty ref")
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("ref %q starts with -", s)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return fmt.Errorf("ref %q has a character git doesn't allow in a ref", s)
		}
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") || s == "@" {
		return fmt.Errorf("ref %q isn't a valid git ref", s)
	}
	return nil
}
