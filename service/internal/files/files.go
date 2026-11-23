// Package files reads the visible, non-secret files below an agent's home.
package files

import (
	"errors"
	"time"
)

// Kinds of an entry.
const (
	KindFolder = "folder"
	KindFile   = "file"
	KindOther  = "other" // a socket, pipe, device, or a link that leads nowhere the viewer reads
)

// Entry describes a folder entry without reading its contents.
type Entry struct {
	Name     string
	Kind     string    // KindFolder, KindFile or KindOther
	Link     bool      // the entry itself is a symbolic link
	Size     int64     // a file's size in bytes; 0 for others
	Modified time.Time // the modification time of the target, when readable
	Withheld bool      // proto.WithheldName(Name)
}

// Folder is a bounded, sorted listing.
type Folder struct {
	Path    string  // the real path below the root, empty for the root
	Entries []Entry // at most maxEntries
	More    int     // listed names left out past maxEntries
	Partial bool    // more than maxNames names were present
}

// File is a bounded read of a regular file.
type File struct {
	Path     string // the real path below the root
	Size     int64
	Modified time.Time
	Data     []byte // the whole file, nil when Over
	Over     bool   // larger than the requested limit
}

// Errors describe paths the viewer cannot read.
var (
	ErrNotFound  = errors.New("nothing is there")
	ErrOutside   = errors.New("it leads outside the agent's home")
	ErrHidden    = errors.New("its real path goes through a hidden or withheld name")
	ErrNotFolder = errors.New("it is not a folder")
	ErrNotFile   = errors.New("it is not a regular file")
)

// MaxLinks is how many symbolic links one path may follow.
const MaxLinks = 40

// ErrBadPath is a path proto.CheckFilePath refuses.
var ErrBadPath = errors.New("the path is not one the viewer reads")

// codedError keeps a helper's message and the error it stands for.
type codedError struct {
	msg  string
	base error
}

// Error is the original message, without a second sentinel description.
func (e codedError) Error() string { return e.msg }

// Unwrap is the sentinel the helper named.
func (e codedError) Unwrap() error { return e.base }

// fileCodes names the errors that cross the helper boundary.
var fileCodes = []struct {
	code string
	base error
}{
	{"notfound", ErrNotFound}, {"outside", ErrOutside}, {"hidden", ErrHidden},
	{"notfolder", ErrNotFolder}, {"notfile", ErrNotFile}, {"path", ErrBadPath},
}

// Code is the helper word for a known error, or empty for another error.
func Code(err error) string {
	for _, c := range fileCodes {
		if errors.Is(err, c.base) {
			return c.code
		}
	}
	return ""
}

// FromCode restores a helper's message and sentinel; unknown codes return false.
func FromCode(code, message string) (error, bool) {
	for _, c := range fileCodes {
		if c.code == code {
			return codedError{msg: message, base: c.base}, true
		}
	}
	return nil, false
}
