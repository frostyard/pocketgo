package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// WorkersFile is the owner's ordered list of worker models, in the profile
// folder, which agents' commands see read-only (ADR-0022).
const WorkersFile = "workers.json"

// MaxWorkers bounds the list.
const MaxWorkers = 8

// DefaultWorkers is the list when the folder has no WorkersFile: DeepSeek's
// cheapest model.
var DefaultWorkers = []proto.ModelRef{{Provider: "deepseek", ID: "deepseek-flash"}}

// Workers is the owner's list of worker models, in order of preference.
type Workers struct {
	Models []proto.ModelRef
	// Default says the folder has no list: Models is DefaultWorkers.
	Default bool
}

// String names the list's models in order.
func (w Workers) String() string {
	names := make([]string, len(w.Models))
	for i, m := range w.Models {
		names[i] = m.String()
	}
	return strings.Join(names, ", ")
}

// LoadWorkers reads the worker list in dir: DefaultWorkers when there is no
// file. A file that is no regular file of its own, or breaks a rule of
// ParseWorkers, is an error: it starts no worker.
func LoadWorkers(dir string) (Workers, error) {
	data, err := readProfile(filepath.Join(dir, WorkersFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Workers{Models: slices.Clone(DefaultWorkers), Default: true}, nil
	}
	if err != nil {
		return Workers{}, fmt.Errorf("%s: %w", WorkersFile, err)
	}
	w, err := ParseWorkers(data)
	if err != nil {
		return Workers{}, fmt.Errorf("%s: %w", WorkersFile, err)
	}
	return w, nil
}

// ParseWorkers checks a worker list: {"models": ["provider/id", …]}, 1 to
// MaxWorkers models, none named twice, and no other field. The object's
// keys are read as written: one "models", spelled so, and nothing else,
// since a decoder matches a field's name in any case and keeps the last of
// a key named twice (Codex review of #33).
func ParseWorkers(data []byte) (Workers, error) {
	const shape = `not {"models": ["provider/id", …]}`
	if len(data) > maxProfileSize {
		return Workers{}, fmt.Errorf("the file is larger than %d bytes", maxProfileSize)
	}
	if !utf8.Valid(data) {
		return Workers{}, errors.New("the file is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return Workers{}, fmt.Errorf("%s: it is no object", shape)
	}
	var names *[]string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Workers{}, fmt.Errorf("%s: %w", shape, err)
		}
		key, _ := tok.(string)
		switch {
		case key != "models":
			return Workers{}, fmt.Errorf("%s: it has the field %q", shape, key)
		case names != nil:
			return Workers{}, fmt.Errorf("%s: it names models twice", shape)
		}
		var list []string
		if err := dec.Decode(&list); err != nil {
			return Workers{}, fmt.Errorf("%s: %w", shape, err)
		}
		names = &list
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return Workers{}, fmt.Errorf("%s: the object does not end", shape)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Workers{}, errors.New("text follows the list's object")
	}
	if names == nil || *names == nil {
		return Workers{}, errors.New("it names no models: {\"models\": [\"provider/id\", …]}")
	}
	if len(*names) == 0 || len(*names) > MaxWorkers {
		return Workers{}, fmt.Errorf("it names %d models, not 1 to %d", len(*names), MaxWorkers)
	}
	w := Workers{Models: make([]proto.ModelRef, 0, len(*names))}
	for _, name := range *names {
		ref, err := ParseModelRef(name)
		if err != nil {
			return Workers{}, err
		}
		if slices.Contains(w.Models, ref) {
			return Workers{}, fmt.Errorf("%s is named twice", ref)
		}
		w.Models = append(w.Models, ref)
	}
	return w, nil
}

// Workers reads the folder's worker list, through the folder pocketd
// pinned at start (Folder.read).
func (f Folder) Workers() (Workers, error) {
	data, err := f.read(WorkersFile)
	if errors.Is(err, fs.ErrNotExist) {
		return Workers{Models: slices.Clone(DefaultWorkers), Default: true}, nil
	}
	if err != nil {
		return Workers{}, fmt.Errorf("%s: %w", WorkersFile, err)
	}
	w, err := ParseWorkers(data)
	if err != nil {
		return Workers{}, fmt.Errorf("%s: %w", WorkersFile, err)
	}
	return w, nil
}
