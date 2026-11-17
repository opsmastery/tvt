package propose

import (
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
