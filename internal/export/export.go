// Package export writes a query result as text: the flat formats (CSV, TSV,
// Markdown, JSON Lines) and the document formats (JSON, YAML, TOON).
package export

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

// Format is an export format name, as /api/query's format field takes it.
type Format string

// The export formats.
const (
	CSV   Format = "csv"
	TSV   Format = "tsv"
	MD    Format = "md"
	JSON  Format = "json"
	JSONL Format = "jsonl"
	YAML  Format = "yaml"
	TOON  Format = "toon"
	SQL   Format = "sql"
)

// ErrUnsupported is returned by Write for a format it cannot write.
var ErrUnsupported = errors.New("format not supported")

var contentTypes = map[Format]string{
	CSV:   "text/csv",
	TSV:   "text/tab-separated-values",
	MD:    "text/markdown",
	JSON:  "application/json",
	JSONL: "application/jsonl",
	YAML:  "application/yaml",
	TOON:  "text/plain",
	SQL:   "application/sql",
}

// ParseFormat checks a format name.
func ParseFormat(s string) (Format, error) {
	f := Format(s)
	if _, ok := contentTypes[f]; !ok {
		return "", fmt.Errorf("unknown format %q: want csv, tsv, md, json, jsonl, yaml, toon or sql", s)
	}
	return f, nil
}

// FormatFromPath picks the format for an output file by its extension
// (case-insensitive; .yml is YAML). It reports false for any other.
func FormatFromPath(path string) (Format, bool) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "yml" {
		return YAML, true
	}
	f, err := ParseFormat(ext)
	return f, err == nil
}

// ContentType is the HTTP Content-Type of the format, with charset.
func (f Format) ContentType() string {
	return contentTypes[f] + "; charset=utf-8"
}

// Write writes res in format f. s is the schema of the queried stream.
func Write(w io.Writer, f Format, res *query.Result, s schema.Schema) error {
	switch f {
	case CSV, TSV, MD, JSONL:
		return writeFlat(w, f, res)
	case JSON, YAML, TOON:
		return writeDoc(w, f, res, s)
	}
	return fmt.Errorf("%w: %s", ErrUnsupported, f)
}

// Note describes what the result left out (Samples cut at the row limit,
// Table groups cut by top, Graph groups folded into "other"), for stderr or
// the X-Loghorn-Note header. It is "" when nothing was cut.
func Note(res *query.Result, q query.Query) string {
	switch res.View {
	case "table":
		if res.Groups > len(res.Rows) {
			return fmt.Sprintf("%d groups matched, showing the top %d", res.Groups, len(res.Rows))
		}
	case "graph":
		if g := res.Graph; g != nil {
			for _, s := range g.Series {
				if s.Other {
					return fmt.Sprintf("%d groups matched, %d drawn on their own, the rest folded into \"other\"", g.Groups, g.Top)
				}
			}
		}
	case "heatmap":
	default:
		if res.Truncated {
			return fmt.Sprintf("%d records matched, showing %d", res.Matched, len(res.Rows))
		}
	}
	return ""
}

// BandName names a heatmap duration band: "<1 ms" for the lowest band (it
// also holds zero and negative durations), else "1-2.15 ms".
func BandName(b query.Band, lowest bool) string {
	if lowest {
		return "<" + num(b.Hi) + " ms"
	}
	return num(b.Lo) + "-" + num(b.Hi) + " ms"
}

// num prints a float in the shortest form that round-trips.
func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
