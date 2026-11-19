package schema

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/BurntSushi/toml"
)

// ErrUnsavableStream is returned when a stream name cannot be used as a file
// name under .config/loghorn/schemas.
var ErrUnsavableStream = errors.New("stream name cannot be saved")

var streamRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidStream reports whether stream may be saved.
func ValidStream(stream string) bool { return streamRe.MatchString(stream) }

var validTypes = map[Type]bool{
	TInt: true, TFloat: true, TBool: true, TString: true,
	TDuration: true, TTimestamp: true, TJSON: true,
}

var validVisualizers = map[string]bool{
	VRaw: true, VNumber: true, VLocalTime: true, VDuration: true,
	VSeverity: true, VSQL: true, VJSON: true, VPlain: true, VLink: true,
}

// validateField checks a field's Type, Visualizer and Unit against the
// constants; the error names the field.
func validateField(f Field) error {
	if !validTypes[f.Type] {
		return fmt.Errorf("field %q: unknown type %q", f.Path, f.Type)
	}
	if !validVisualizers[f.Visualizer] {
		return fmt.Errorf("field %q: unknown visualizer %q", f.Path, f.Visualizer)
	}
	if f.Width < 0 {
		return fmt.Errorf("field %q: negative width %d", f.Path, f.Width)
	}
	if !ValidUnit(f.Type, f.Unit) {
		return fmt.Errorf("field %q: invalid unit %q for type %s", f.Path, f.Unit, f.Type)
	}
	return nil
}

// FindConfigDir walks up from start looking for a .config/loghorn directory,
// stopping after the git root (the first directory containing .git, as a
// directory or a file). When none is found it returns the directory that
// would hold it (<gitroot>/.config/loghorn, or <start>/.config/loghorn outside
// git) with found=false and creates nothing. It never consults $HOME or
// $XDG_CONFIG_HOME.
func FindConfigDir(start string) (dir string, found bool, err error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", false, err
	}
	// Find the git root first; outside git only start itself is considered, so
	// an ancestor such as ~/.config/loghorn is never picked up.
	gitRoot := ""
	for cur := abs; ; {
		if _, e := os.Stat(filepath.Join(cur, ".git")); e == nil {
			gitRoot = cur
			break
		} else if !errors.Is(e, os.ErrNotExist) {
			return "", false, e
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if gitRoot == "" {
		gitRoot = abs
	}
	for cur := abs; ; {
		cand := filepath.Join(cur, ".config", "loghorn")
		if st, e := os.Stat(cand); e == nil && st.IsDir() {
			return cand, true, nil
		}
		if cur == gitRoot {
			break
		}
		cur = filepath.Dir(cur)
	}
	return filepath.Join(gitRoot, ".config", "loghorn"), false, nil
}

func schemaPath(dir, stream string) string {
	return filepath.Join(dir, "schemas", stream+".toml")
}

// LoadSaved reads dir/schemas/<stream>.toml. It returns nil, nil when the
// file is absent or the stream name is not saveable.
func LoadSaved(dir, stream string) (*Schema, error) {
	if !ValidStream(stream) {
		return nil, nil
	}
	path := schemaPath(dir, stream)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Schema
	if err := toml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range s.Fields {
		// A hand-edited file may leave the visualizer out; use the default
		// for the field's type rather than locking out the whole stream.
		if f := &s.Fields[i]; f.Visualizer == "" {
			f.Visualizer = DefaultVisualizer(f.Path, f.Type)
		}
		if err := validateField(s.Fields[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if s.Stream == "" {
		s.Stream = stream
	}
	sort.Slice(s.Fields, func(i, j int) bool { return s.Fields[i].Path < s.Fields[j].Path })
	for i := range s.Fields {
		s.Fields[i].Saved = true
	}
	return &s, nil
}

// SaveSchema writes s to dir/schemas/<stream>.toml via a temp file and rename
// in the same directory, and returns the path.
func SaveSchema(dir string, s Schema) (path string, err error) {
	if !ValidStream(s.Stream) {
		return "", ErrUnsavableStream
	}
	for _, f := range s.Fields {
		if err := validateField(f); err != nil {
			return "", err
		}
	}
	sdir := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(sdir, ".schema-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := toml.NewEncoder(tmp).Encode(s); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path = schemaPath(dir, s.Stream)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// Effective merges a saved schema over an inferred one. Saved
// Type/Unit/Visualizer/Label/Hidden/Width/Wrap win for fields present in both; saved
// fields not seen in the data are kept with Count 0; inferred fields absent
// from a non-nil saved schema are flagged New. Hidden fields stay in Fields
// but are dropped from the columns.
func Effective(inferred Schema, saved *Schema) Schema {
	out := Schema{Stream: inferred.Stream}
	byPath := map[string]int{}
	for _, f := range inferred.Fields {
		byPath[f.Path] = len(out.Fields)
		out.Fields = append(out.Fields, f)
	}
	if saved != nil {
		seen := map[string]bool{}
		for _, sf := range saved.Fields {
			seen[sf.Path] = true
			if i, ok := byPath[sf.Path]; ok {
				f := out.Fields[i]
				f.Type, f.Unit, f.Visualizer, f.Label, f.Hidden, f.Width, f.Wrap = sf.Type, sf.Unit, sf.Visualizer, sf.Label, sf.Hidden, sf.Width, sf.Wrap
				f.Saved = true
				out.Fields[i] = f
				continue
			}
			sf.Count, sf.New, sf.Saved = 0, false, true
			out.Fields = append(out.Fields, sf)
		}
		for i := range out.Fields {
			if !seen[out.Fields[i].Path] {
				out.Fields[i].New = true
			}
		}
	}
	sort.Slice(out.Fields, func(i, j int) bool { return out.Fields[i].Path < out.Fields[j].Path })

	cols := inferred.Columns
	if saved != nil && len(saved.Columns) > 0 {
		cols = saved.Columns
	}
	hidden := map[string]bool{}
	for _, f := range out.Fields {
		if f.Hidden {
			hidden[f.Path] = true
		}
	}
	for _, c := range cols {
		if !hidden[c] {
			out.Columns = append(out.Columns, c)
		}
	}
	return out
}
