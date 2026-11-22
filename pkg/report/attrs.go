package report

import (
	"fmt"
	"hash/fnv"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// AttrRow is one key's size in one of the largest streams (S10 item 7),
// from a sample of its recent lines: names and sizes only.
type AttrRow struct {
	ID     string `json:"id"`
	Stream string `json:"stream"`
	Key    string `json:"key"`
	// Share is how many sampled lines carry the key.
	Share       float64 `json:"share"`
	BytesPerDay int64   `json:"bytes_per_day"`
	MaxBytes    int     `json:"max_bytes"`
	Distinct    int     `json:"distinct"`
	// Read is lines (a reader returns the stream's lines), named (a reader
	// names the key) or empty; Reader is one that does.
	Read   string `json:"read,omitempty"`
	Reader string `json:"reader,omitempty"`
	// Cap is the saving from cutting the key's values to 512 bytes plus a
	// marker, for a key nothing reads; absent otherwise. Action says what
	// to do, and why not when the key is read.
	Cap       *Savings `json:"cap,omitempty"`
	Action    string   `json:"action"`
	Sensitive string   `json:"sensitive,omitempty"`
}

// attrRowsMax caps the attribute rows in the report, largest first.
const attrRowsMax = 30

// attrRows reports the largest keys. A key nothing reads with values past
// the cap gets the cap's saving: ADR 0010 puts a cap ahead of every other
// log action. tvt doesn't write caps yet.
func attrRows(as []graph.StreamAttr, currency string) []AttrRow {
	out := []AttrRow{}
	for _, a := range as {
		if len(out) == attrRowsMax {
			break
		}
		if a.BytesPerDay <= 0 {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(a.Stream + "\x00" + a.Key))
		row := AttrRow{ID: fmt.Sprintf("attr-%08x", h.Sum32()), Stream: a.Stream, Key: a.Key,
			BytesPerDay: a.BytesPerDay, MaxBytes: a.MaxBytes, Distinct: a.Distinct, Read: a.Read, Reader: a.Reader,
			Sensitive: SensitiveName(a.Key)}
		if a.SampledLines > 0 {
			row.Share = float64(a.Lines) / float64(a.SampledLines)
		}
		switch {
		case a.Read == graph.AttrReadLines:
			row.Action = "Kept: " + a.Reader + " returns the stream's lines, so it shows this key."
		case a.Read == graph.AttrReadNamed:
			row.Action = "Kept: " + a.Reader + " reads this key."
		case a.CapBytesPerDay > 0:
			row.Cap = &Savings{Monthly: a.CapMonthly, Currency: currency, Approximate: true}
			row.Action = fmt.Sprintf("Nothing reads it, and values reach %d bytes: cap them at 512 bytes with a marker. tvt doesn't write caps yet.", a.MaxBytes)
		default:
			row.Action = "Nothing reads it; its values stay under the 512-byte cap."
		}
		out = append(out, row)
	}
	return out
}
