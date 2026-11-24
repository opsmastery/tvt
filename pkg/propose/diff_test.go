package propose

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiff: git apply takes every diff Diff writes and gets the new file.
func TestDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	lines := func(n int, f func(int) string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(f(i) + "\n")
		}
		return b.String()
	}
	base := lines(40, func(i int) string { return "line " + string(rune('a'+i%26)) + strings.Repeat("x", i) })
	for _, c := range []struct{ name, old, new string }{
		{"insert in the middle", base, strings.Replace(base, "line kxxxxxxxxxx\n", "line kxxxxxxxxxx\n# tvt:begin x\n- rule\n# tvt:end\n", 1)},
		{"two hunks", base, strings.Replace(strings.Replace(base, "line b", "line B", 1), "line z", "line Z", 1)},
		{"delete", base, strings.Replace(base, "line cxx\n", "", 1)},
		{"at the start", base, "first\n" + base},
		{"at the end", base, base + "last\n"},
		{"no newline at end", "a\nb", "a\nb\nc"},
		{"into an empty file", "", "new\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Diff("dir/f.txt", []byte(c.old), []byte(c.new))
			if d == "" {
				t.Fatal("empty diff")
			}
			dir := t.TempDir()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.MkdirAll(filepath.Join(dir, "dir"), 0o755))
			must(os.WriteFile(filepath.Join(dir, "dir", "f.txt"), []byte(c.old), 0o644))
			must(os.WriteFile(filepath.Join(dir, "p.patch"), []byte(d), 0o644))
			cmd := exec.Command("git", "apply", "p.patch")
			cmd.Dir = dir
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git apply: %v\n%s\n%s", err, b, d)
			}
			got, err := os.ReadFile(filepath.Join(dir, "dir", "f.txt"))
			must(err)
			if string(got) != c.new {
				t.Errorf("applied:\n%s\nwant:\n%s\ndiff:\n%s", got, c.new, d)
			}
		})
	}
	if d := Diff("f", []byte("same\n"), []byte("same\n")); d != "" {
		t.Errorf("equal inputs: %q", d)
	}
}

// A middle too large for the LCS table becomes one replacement hunk that
// still applies: old lines out, new lines in, context kept.
func TestDiffLargeMiddleIsBounded(t *testing.T) {
	var old, new strings.Builder
	old.WriteString("head\n")
	new.WriteString("head\n")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&old, "old %d\n", i)
		fmt.Fprintf(&new, "new %d\n", i)
	}
	old.WriteString("tail\n")
	new.WriteString("tail\n")
	d := Diff("big.yaml", []byte(old.String()), []byte(new.String()))
	if !strings.Contains(d, "@@ -1,3002 +1,3002 @@\n head\n-old 0\n") {
		t.Fatalf("unexpected hunk header or start:\n%.200s", d)
	}
	if strings.Count(d, "\n-old ") != 3000 || strings.Count(d, "\n+new ") != 3000 {
		t.Fatal("every old line should be removed and every new line added")
	}
	if !strings.HasSuffix(d, "+new 2999\n tail\n") {
		t.Fatalf("diff should end with the last new line and the tail context:\n%s", d[len(d)-80:])
	}
}
