package pig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ContextHelper is the hidden pocketd command Engine.loadContext runs in
// the sandbox to read what a session's instructions hold (ADR-0043): the
// files and the skills folders its one argument, a contextRequest in JSON,
// names. It answers with a contextAnswer in JSON. It reads each file as
// ReadHelper does: opened without waiting for a writer, its type checked on
// the descriptor it reads, links followed as the sandbox shows them, so a
// link leads nowhere the agents' own tools could not read, and a folder
// the sandbox hides reads as nothing.
const ContextHelper = "__read-context"

// contextRequest is what ContextHelper reads: each file up to its bound
// and one byte more, and in each skills folder the SKILL.md of each of its
// first folders, by name, up to the skills' bound and one byte more.
type contextRequest struct {
	Files  []fileRequest   `json:"files"`
	Skills []skillsRequest `json:"skills"`
}

type fileRequest struct {
	Path string `json:"path"`
	Max  int64  `json:"max"`
}

type skillsRequest struct {
	Dir string `json:"dir"`
	// Max is how many skills are read, the first by name; Head bounds
	// each SKILL.md.
	Max  int   `json:"max"`
	Head int64 `json:"head"`
}

// contextAnswer is ContextHelper's answer, one entry for each of the
// request's, in its order.
type contextAnswer struct {
	Files  []fileAnswer   `json:"files"`
	Skills []skillsAnswer `json:"skills"`
}

type fileAnswer struct {
	Found bool   `json:"found,omitempty"`
	Data  []byte `json:"data,omitempty"`
	Names uint64 `json:"names,omitempty"`
	Err   string `json:"err,omitempty"`
}

type skillsAnswer struct {
	Found  bool          `json:"found,omitempty"`
	Skills []skillAnswer `json:"skills,omitempty"`
	More   bool          `json:"more,omitempty"`
	Err    string        `json:"err,omitempty"`
}

type skillAnswer struct {
	Name string     `json:"name"`
	File fileAnswer `json:"file"`
}

// RunContextHelper is ContextHelper's work, with its arguments after the
// command's name, writing its answer to w: its exit status, 0 for an
// answer, 2 for arguments it does not take, 1 for an answer it could not
// write.
func RunContextHelper(args []string, w io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	var request contextRequest
	if err := json.Unmarshal([]byte(args[0]), &request); err != nil {
		return 2
	}
	answer := contextAnswer{Files: []fileAnswer{}, Skills: []skillsAnswer{}}
	for _, f := range request.Files {
		answer.Files = append(answer.Files, readContextFile(f.Path, f.Max+1))
	}
	for _, s := range request.Skills {
		answer.Skills = append(answer.Skills, readSkills(s))
	}
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// unseen are the errors of a path that is no file the sandbox shows: not
// there, not allowed, a loop of links, a file on the way, or a socket.
func unseen(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENXIO)
}

// readContextFile is the regular file at path, at most max bytes of it.
func readContextFile(path string, max int64) fileAnswer {
	f, err := openNonblocking(path)
	switch {
	case unseen(err):
		return fileAnswer{}
	case err != nil:
		return fileAnswer{Err: err.Error()}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fileAnswer{Err: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return fileAnswer{}
	}
	data, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return fileAnswer{Err: err.Error()}
	}
	return fileAnswer{Found: true, Data: data, Names: names(info)}
}

// readSkills is the skills folder s.Dir: the SKILL.md of each of its
// folders, by name, that has one, the first s.Max of them. A name that
// starts with a dot is no skill's, nor is a file.
func readSkills(s skillsRequest) skillsAnswer {
	entries, err := os.ReadDir(s.Dir)
	switch {
	case unseen(err):
		return skillsAnswer{}
	case err != nil:
		return skillsAnswer{Err: err.Error()}
	}
	out := skillsAnswer{Found: true}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		file := readContextFile(filepath.Join(s.Dir, name, "SKILL.md"), s.Head+1)
		if !file.Found && file.Err == "" {
			continue
		}
		if len(out.Skills) == s.Max {
			out.More = true
			break
		}
		out.Skills = append(out.Skills, skillAnswer{Name: name, File: file})
	}
	return out
}
