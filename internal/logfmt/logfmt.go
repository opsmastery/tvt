// Package logfmt parses logfmt lines (key=value pairs, values optionally
// double-quoted with backslash escapes), as written by Grafana, Loki and
// Mimir.
package logfmt

// Parse returns the key/value pairs in line. A bare key has an empty value.
// When a key repeats, the last value wins. Parse never fails: malformed
// input yields whatever pairs could be read, so callers check for the keys
// they need.
func Parse(line []byte) map[string]string {
	out := map[string]string{}
	i, n := 0, len(line)
	for i < n {
		for i < n && line[i] == ' ' {
			i++
		}
		start := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}
		key := string(line[start:i])
		if i >= n || line[i] == ' ' {
			if key != "" {
				out[key] = ""
			}
			continue
		}
		i++ // '='
		var val []byte
		if i < n && line[i] == '"' {
			i++
			for i < n && line[i] != '"' {
				if line[i] == '\\' && i+1 < n {
					i++
					switch line[i] {
					case 'n':
						val = append(val, '\n')
					case 't':
						val = append(val, '\t')
					default:
						val = append(val, line[i])
					}
				} else {
					val = append(val, line[i])
				}
				i++
			}
			i++ // closing quote
		} else {
			start := i
			for i < n && line[i] != ' ' {
				i++
			}
			val = line[start:i]
		}
		if key != "" {
			out[key] = string(val)
		}
	}
	return out
}
