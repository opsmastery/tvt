package safe

import "testing"

func TestRepoPath(t *testing.T) {
	for _, p := range []string{"prometheus.yml", "deploy/otel/config.yaml", ".tvt/protect.json", "a/./b.yaml"} {
		if err := RepoPath(p); err != nil {
			t.Errorf("RepoPath(%q) = %v, want ok", p, err)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x.yaml", "a/../../x", "..", `a\b.yaml`, "a\x00b"} {
		if err := RepoPath(p); err == nil {
			t.Errorf("RepoPath(%q) ok, want an error", p)
		}
	}
}

func TestRef(t *testing.T) {
	for _, s := range []string{"main", "origin/main", "tvt/drop-2026-11-22", "release-1.2", "HEAD"} {
		if err := Ref(s); err != nil {
			t.Errorf("Ref(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range []string{"", "-x", "--upload-pack=touch /tmp/p", "a b", "a..b", "a:b", "a~1", "x@{1}", "a/", "a.lock", "a\tb", "@"} {
		if err := Ref(s); err == nil {
			t.Errorf("Ref(%q) ok, want an error", s)
		}
	}
}
