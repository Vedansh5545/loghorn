package ingest

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Stored reads one of loghorn's own stored log files to EOF, invoking emit
// once per record.
//
// Records can't frame a stored file, because it decides from the first line
// alone, and one stored day can hold several runs from different producers:
// a run of one-JSON-object-per-line followed by a run of YAML documents came
// back as every YAML line a separate entry. Stored instead frames the file
// line by line, which CompactRecord's invariant allows: every record is on
// one line except a YAML document, which keeps its newlines and begins "--".
//
// So a line beginning "--" starts a document, and the lines after it continue
// it while they could be YAML: indented, a "- " sequence item, or a top-level
// "key:". Any other line ends the document and is a record of its own. Blank
// lines join the document only if more of it follows. A plain-text line that
// happens to read as "word: ..." directly after a document is taken into it;
// that is the price of framing without a terminator.
//
// The slice passed to emit is only valid during the call; emit must copy it
// to retain it.
func Stored(r io.Reader, emit func(rec []byte)) error {
	br := bufio.NewReader(r)
	var doc, blanks [][]byte
	flush := func() {
		if len(doc) > 0 {
			emit(bytes.Join(doc, []byte("\n")))
			doc = nil
		}
		for _, b := range blanks {
			emit(b)
		}
		blanks = nil
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			t := trimEOL(line)
			switch {
			case isDocSeparator(t):
				flush()
				doc = append(doc, t)
			case doc != nil && len(bytes.TrimSpace(t)) == 0:
				blanks = append(blanks, t)
			case doc != nil && continuesYAML(t):
				doc = append(append(doc, blanks...), t)
				blanks = nil
			default:
				flush()
				emit(t)
			}
		}
		if err != nil {
			flush()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// continuesYAML reports whether line can belong to a YAML document already
// under way: indented, a top-level sequence item, or a top-level mapping key.
func continuesYAML(line []byte) bool {
	switch {
	case len(line) == 0:
		return false
	case line[0] == ' ' || line[0] == '\t':
		return true
	case line[0] == '-' && (len(line) == 1 || line[1] == ' '):
		return true
	}
	for i, c := range line {
		switch {
		case c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i > 0 && (c == '.' || c == '-' || '0' <= c && c <= '9'):
		case i > 0 && c == ':':
			return i+1 == len(line) || line[i+1] == ' '
		default:
			return false
		}
	}
	return false
}
