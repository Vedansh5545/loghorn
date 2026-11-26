package export

import (
	"bufio"
	"fmt"
	"io"
)

// WriteRows writes the rows of a SQL answer, columns cols, in format f:
// csv, tsv, md, jsonl, or json (an array of objects). Values are export
// values (nil, string, int64, float64, bool, json.Number, json.RawMessage).
func WriteRows(w io.Writer, f Format, cols []string, rows [][]any) error {
	t := table{header: cols, rows: rows}
	switch f {
	case CSV:
		return writeCSV(w, t)
	case TSV:
		return writeLines(w, t, tsvEscape, "\t", "", "")
	case MD:
		return writeMD(w, t)
	case JSONL:
		return writeJSONL(w, t)
	case JSON:
		return writeJSONArray(w, t)
	}
	return fmt.Errorf("%w for SQL: %s (want tsv, csv, md, json or jsonl)", ErrUnsupported, f)
}

// writeJSONArray writes the rows as a JSON array of objects, one per line.
func writeJSONArray(w io.Writer, t table) error {
	bw := bufio.NewWriter(w)
	bw.WriteString("[")
	for i, row := range t.rows {
		if i > 0 {
			bw.WriteString(",")
		}
		bw.WriteString("\n")
		writeObject(bw, t.header, row)
	}
	if len(t.rows) > 0 {
		bw.WriteString("\n")
	}
	bw.WriteString("]\n")
	return bw.Flush()
}

// writeObject writes one row as a JSON object, keys in column order.
func writeObject(bw *bufio.Writer, header []string, row []any) {
	bw.WriteByte('{')
	for i, h := range header {
		if i > 0 {
			bw.WriteByte(',')
		}
		bw.Write(jsonText(h))
		bw.WriteByte(':')
		var v any
		if i < len(row) {
			v = row[i]
		}
		bw.Write(jsonValue(v))
	}
	bw.WriteByte('}')
}
