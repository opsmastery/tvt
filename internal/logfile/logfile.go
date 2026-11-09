// Package logfile reads line-oriented log files the way collectors need
// them: glob patterns, rotated files, gzip, no line-length limit, and
// cancellation.
package logfile

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Expand resolves paths (files or glob patterns) to a sorted, de-duplicated
// list of files. A pattern that matches nothing is an error: a missing log
// must not look like an empty one.
func Expand(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("no log paths configured")
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range paths {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", p, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%s: no such file", p)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

// ScanFile calls fn with every non-blank line of path, trimmed of
// surrounding whitespace. Files ending in .gz are decompressed. The slice
// passed to fn is only valid during the call.
func ScanFile(ctx context.Context, path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}

	br := bufio.NewReaderSize(r, 64*1024)
	for n := 0; ; n++ {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		// ReadBytes has no line-length limit, unlike bufio.Scanner.
		raw, err := br.ReadBytes('\n')
		if line := bytes.TrimSpace(raw); len(line) > 0 {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
