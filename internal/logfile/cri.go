package logfile

import (
	"bytes"
	"context"
	"regexp"
)

// criPrefix is the CRI container log format Kubernetes nodes write
// (/var/log/pods/...) and Podman's k8s-file log driver uses:
// "<RFC 3339 time> <stdout|stderr> <F|P> <content>", P marking a partial
// line continued by the next entry.
var criPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[0-9:.]+(?:Z|[+-]\d{2}:\d{2}) (?:stdout|stderr) ([FP]) `)

// ScanContainerFile is ScanFile for a backend's own log, which may be
// stored in the CRI container log format: such lines are unwrapped (and
// partial ones joined) before fn sees them; other lines pass unchanged, so
// plain log files work too. Lines are trimmed as in ScanFile, but only
// after joining: a partial chunk can end in a space that belongs to the line.
func ScanContainerFile(ctx context.Context, path string, fn func(line []byte)) error {
	var partial []byte
	err := scan(ctx, path, func(raw []byte) {
		line := bytes.TrimRight(raw, "\r\n")
		m := criPrefix.FindSubmatchIndex(line)
		if m == nil {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				return
			}
			if partial != nil {
				fn(partial) // a partial line cut off by rotation
				partial = nil
			}
			fn(line)
			return
		}
		content := line[m[1]:]
		if line[m[2]] == 'P' {
			partial = append(partial, content...)
			return
		}
		if partial != nil {
			content = append(partial, content...)
			partial = nil
		}
		if content = bytes.TrimSpace(content); len(content) > 0 {
			fn(content)
		}
	})
	if err == nil && partial != nil {
		fn(partial)
	}
	return err
}
