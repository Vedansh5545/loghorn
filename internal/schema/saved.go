package schema

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
)

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
	// Type and Visualizer may be empty: a saved field then leaves the inferred
	// value alone, so a width-only edit never pins the kind's inferred type.
	if f.Type != "" && !validTypes[f.Type] {
		return fmt.Errorf("field %q: unknown type %q", f.Path, f.Type)
	}
	if f.Visualizer != "" && !validVisualizers[f.Visualizer] {
		return fmt.Errorf("field %q: unknown visualizer %q", f.Path, f.Visualizer)
	}
	if f.Width < 0 {
		return fmt.Errorf("field %q: negative width %d", f.Path, f.Width)
	}
	if f.Type != "" && !ValidUnit(f.Type, f.Unit) {
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

// ErrBadSource is returned for a source that is neither log nor otel.
var ErrBadSource = errors.New("unknown source")

func fieldsPath(dir, source string) (string, error) {
	src, ok := record.ParseSource(source)
	if !ok {
		return "", ErrBadSource
	}
	return filepath.Join(dir, "fields-"+src+".toml"), nil
}

type fieldsFile struct {
	Fields []Field `toml:"field"`
}

// LoadFields reads dir/fields-<source>.toml (log or otel). An absent file is
// nil, nil. Fields are validated, sorted by path and flagged Saved.
func LoadFields(dir, source string) ([]Field, error) {
	path, err := fieldsPath(dir, source)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f fieldsFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range f.Fields {
		// A hand-edited file may leave the visualizer out; use the default
		// for the field's type rather than locking out the whole file.
		if x := &f.Fields[i]; x.Visualizer == "" && x.Type != "" {
			x.Visualizer = DefaultVisualizer(x.Path, x.Type)
		}
		if err := validateField(f.Fields[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		f.Fields[i].Saved = true
	}
	sort.Slice(f.Fields, func(i, j int) bool { return f.Fields[i].Path < f.Fields[j].Path })
	return f.Fields, nil
}

// MergeFields replaces or adds each of fields (by Path) in
// dir/fields-<source>.toml, leaving every other saved field alone, and returns
// the file path. The input is validated and the existing file read first; an
// error from either writes nothing. Callers serialise concurrent merges.
func MergeFields(dir, source string, fields []Field) (path string, err error) {
	path, err = fieldsPath(dir, source)
	if err != nil {
		return "", err
	}
	for _, f := range fields {
		if err := validateField(f); err != nil {
			return "", err
		}
	}
	cur, err := LoadFields(dir, source)
	if err != nil {
		return "", err
	}
	byPath := map[string]int{}
	for i, f := range cur {
		byPath[f.Path] = i
	}
	for _, f := range fields {
		if i, ok := byPath[f.Path]; ok {
			// An empty type or visualizer means "not edited": keep the saved one.
			if f.Type == "" {
				f.Type = cur[i].Type
			}
			if f.Visualizer == "" {
				f.Visualizer = cur[i].Visualizer
			}
			cur[i] = f
			continue
		}
		byPath[f.Path] = len(cur)
		cur = append(cur, f)
	}
	sort.Slice(cur, func(i, j int) bool { return cur[i].Path < cur[j].Path })
	err = svcconf.WriteAtomic(path, func(w io.Writer) error {
		return toml.NewEncoder(w).Encode(fieldsFile{Fields: cur})
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// Effective applies saved field settings over an inferred schema. Saved
// Type/Unit/Visualizer/Label/Hidden/Width/Wrap win for fields present in both
// (flagged Saved); saved paths absent from inferred are ignored, since the
// saved file is source-wide. Columns are inferred.Columns minus hidden fields.
func Effective(inferred Schema, saved []Field) Schema {
	out := Schema{Stream: inferred.Stream, Source: inferred.Source}
	byPath := map[string]int{}
	for _, f := range inferred.Fields {
		byPath[f.Path] = len(out.Fields)
		out.Fields = append(out.Fields, f)
	}
	for _, sf := range saved {
		if i, ok := byPath[sf.Path]; ok {
			f := &out.Fields[i]
			// Only a set type or visualizer overrides the inferred one, so
			// settings saved from one kind don't retype the same path in another.
			if sf.Type != "" {
				f.Type = sf.Type
			}
			if sf.Visualizer != "" {
				f.Visualizer = sf.Visualizer
			}
			f.Unit, f.Label, f.Hidden, f.Width, f.Wrap = sf.Unit, sf.Label, sf.Hidden, sf.Width, sf.Wrap
			if !ValidUnit(f.Type, f.Unit) {
				f.Unit = ""
			}
			f.Saved = true
		}
	}
	sort.Slice(out.Fields, func(i, j int) bool { return out.Fields[i].Path < out.Fields[j].Path })
	hidden := map[string]bool{}
	for _, f := range out.Fields {
		if f.Hidden {
			hidden[f.Path] = true
		}
	}
	for _, c := range inferred.Columns {
		if !hidden[c] {
			out.Columns = append(out.Columns, c)
		}
	}
	return out
}
