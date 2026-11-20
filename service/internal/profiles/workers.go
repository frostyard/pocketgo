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
// MaxWorkers models, none named twice, and no other field.
func ParseWorkers(data []byte) (Workers, error) {
	if len(data) > maxProfileSize {
		return Workers{}, fmt.Errorf("the file is larger than %d bytes", maxProfileSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file struct {
		Models *[]string `json:"models"`
	}
	if err := dec.Decode(&file); err != nil {
		return Workers{}, fmt.Errorf("not {\"models\": [\"provider/id\", …]}: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Workers{}, errors.New("text follows the list's object")
	}
	if file.Models == nil {
		return Workers{}, errors.New("it names no models: {\"models\": [\"provider/id\", …]}")
	}
	names := *file.Models
	if len(names) == 0 || len(names) > MaxWorkers {
		return Workers{}, fmt.Errorf("it names %d models, not 1 to %d", len(names), MaxWorkers)
	}
	w := Workers{Models: make([]proto.ModelRef, 0, len(names))}
	for _, name := range names {
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

// Workers reads the folder's worker list.
func (f Folder) Workers() (Workers, error) {
	if err := f.check(); err != nil {
		return Workers{}, err
	}
	return LoadWorkers(f.Dir)
}
