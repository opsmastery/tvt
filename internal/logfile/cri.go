package logfile

import (
	"bytes"
	"context"
	"regexp"
)

// criPrefix is the CRI container log format Kubernetes nodes write
// (/var/log/pods/...) and Podman's k8s-file log driver uses:
// "<RFC 3339 time> <stdout|stderr> <F|P> <content>", P marking a partial
// line continued by the next entry on the same stream.
var criPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[0-9:.]+(?:Z|[+-]\d{2}:\d{2}) (stdout|stderr) ([FP]) `)

// ScanContainerFile is ScanFile for a backend's own log, which may be
// stored in the CRI container log format: such lines are unwrapped (and
// partial ones joined, per stream, since stdout and stderr interleave)
// before fn sees them; other lines pass unchanged, so plain log files work
// too. Lines are trimmed as in ScanFile, but only after joining: a partial
// chunk can end in a space that belongs to the line.
func ScanContainerFile(ctx context.Context, path string, fn func(line []byte)) error {
	partial := map[string][]byte{}
	var pending []string // streams holding a partial line, first fragment first
	flush := func() {    // partial lines cut off by rotation or the end of the file
		for _, s := range pending {
			fn(partial[s])
			delete(partial, s)
		}
		pending = pending[:0]
	}
	err := scan(ctx, path, func(raw []byte) {
		line := bytes.TrimRight(raw, "\r\n")
		m := criPrefix.FindSubmatchIndex(line)
		if m == nil {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				return
			}
			flush()
			fn(line)
			return
		}
		stream, content := string(line[m[2]:m[3]]), line[m[1]:]
		buf, open := partial[stream]
		if line[m[4]] == 'P' {
			if !open {
				pending = append(pending, stream)
			}
			partial[stream] = append(buf, content...)
			return
		}
		if open {
			content = append(buf, content...)
			delete(partial, stream)
			for i, s := range pending {
				if s == stream {
					pending = append(pending[:i], pending[i+1:]...)
					break
				}
			}
		}
		if content = bytes.TrimSpace(content); len(content) > 0 {
			fn(content)
		}
	})
	if err == nil {
		flush()
	}
	return err
}
