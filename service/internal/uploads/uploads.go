// Package uploads keeps the files the owner attaches to messages
// (ADR-0029): in the service user's ~/.local/share/pocketgo-uploads, one
// folder a session, outside pocketd's data folder, where the agents'
// sandbox lets them read the files.
//
// The agents run as pocketd's own user and can write there too, so pocketd
// follows no link in that folder: it opens every folder with os.Root and
// checks it is the folder its name holds, creates a file only where no
// entry is, and reads a file only inside its session's folder, as a
// regular file.
package uploads

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DirName is the uploads folder's name in the service user's
// ~/.local/share.
const DirName = "pocketgo-uploads"

// Dir is the uploads folder of the user whose home is home.
func Dir(home string) string { return filepath.Join(home, ".local", "share", DirName) }

// Folder is the uploads folder: <base>/<session>/<file>. It is safe for
// concurrent use.
type Folder struct {
	base string
	// now and random name each new file (FileName); tests set them.
	now    func() time.Time
	random func() string
}

// New is the uploads folder at base, its new files named at now's time.
func New(base string, now func() time.Time) *Folder {
	if now == nil {
		now = time.Now
	}
	return &Folder{base: filepath.Clean(base), now: now, random: randomHex}
}

// Base is the uploads folder's path.
func (f *Folder) Base() string { return f.base }

// Path is where session's file is.
func (f *Folder) Path(session int64, file string) string {
	return filepath.Join(f.base, strconv.FormatInt(session, 10), file)
}

// randomHex is 8 random hexadecimal characters.
func randomHex() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// FileName is the name a file uploaded as name at at is kept by:
// <stamp>-<random>-<safe name>, its stamp the milliseconds since 1970 in
// base 36, as pi-pocket names one, so files that arrive under one name at
// once, as pasted images do, never meet.
func FileName(at time.Time, random, name string) string {
	return strconv.FormatInt(at.UnixMilli(), 36) + "-" + random + "-" + SafeName(name)
}

// maxName is the most bytes of a safe name, before "upload" when it needs
// one.
const maxName = 120

// SafeName is name as pi-pocket keeps it: its last path element, each run
// of characters other than ASCII letters, digits, "_", ".", "-", space,
// "(" and ")" made one "_", trimmed of spaces and at most 120 bytes, with
// "upload" before it when it is empty or starts with ".". It holds no
// comma, so a message's line names it unmistakably (proto.AttachmentLine).
func SafeName(name string) string {
	name = strings.TrimRight(name, "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	run := false
	for _, r := range name {
		if r < 0x80 && (r == '_' || r == '.' || r == '-' || r == ' ' || r == '(' || r == ')' ||
			r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
			run = false
			continue
		}
		if !run {
			b.WriteByte('_')
			run = true
		}
	}
	safe := strings.TrimSpace(b.String())
	if len(safe) > maxName {
		safe = strings.TrimSpace(safe[:maxName])
	}
	if safe == "" || strings.HasPrefix(safe, ".") {
		safe = "upload" + safe
	}
	return safe
}

// mimeType is a media type, type/subtype, in lowercase.
var mimeType = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// types are the types of the extensions pocketd knows, after pi-pocket's
// table: pocketd never asks the host, so every host gives one name one
// type.
var types = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".heic": "image/heic", ".heif": "image/heif", ".svg": "image/svg+xml", ".bmp": "image/bmp",
	".pdf": "application/pdf", ".json": "application/json", ".zip": "application/zip",
	".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
	".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript",
	".xml": "application/xml", ".yaml": "application/yaml", ".yml": "application/yaml",
}

// Mime is an upload's type: the declared one (a Content-Type) without its
// parameters, in lowercase, when it is a type other than
// application/octet-stream; else the type of name's extension; else
// application/octet-stream.
func Mime(name, declared string) string {
	declared, _, _ = strings.Cut(declared, ";")
	declared = strings.ToLower(strings.TrimSpace(declared))
	if declared != "application/octet-stream" && mimeType.MatchString(declared) {
		return declared
	}
	if t, ok := types[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

// ErrNotItsFolder is a folder of the uploads folder whose name holds a
// link, or anything but the folder it opened: pocketd does not follow it.
var ErrNotItsFolder = errors.New("a link or another entry stands where the uploads folder has a folder")

// open opens the uploads folder's folder for session, from base's parent
// down, making the folders that are missing when create says so. Each
// folder is the one its name holds, never a link (subdir).
func (f *Folder) open(session int64, create bool) (*os.Root, error) {
	parent := filepath.Dir(f.base)
	if create {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, err
		}
	}
	top, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = top.Close() }()
	base, err := subdir(top, filepath.Base(f.base), create)
	if err != nil {
		return nil, err
	}
	defer func() { _ = base.Close() }()
	return subdir(base, strconv.FormatInt(session, 10), create)
}

// subdir opens the folder name of root, made first when create says so,
// and checks that it opened the folder name holds: a link there, even one
// to a folder inside root, or a folder swapped in as it opened, is
// ErrNotItsFolder. root.OpenRoot follows a link that stays inside root;
// Lstat does not, and the two must be one folder.
func subdir(root *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	held, err := root.Lstat(name)
	if err == nil {
		var opened fs.FileInfo
		if opened, err = sub.Stat("."); err == nil && (!held.IsDir() || !os.SameFile(held, opened)) {
			err = fmt.Errorf("%s: %w", name, ErrNotItsFolder)
		}
	}
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	return sub, nil
}

// Store keeps body as session's new upload, named after name
// (FileName), and says its file's name and size. A body that fails to
// read to its end, as a cut-off or too large upload does, leaves no file.
func (f *Folder) Store(session int64, name string, body io.Reader) (file string, size int64, err error) {
	dir, err := f.open(session, true)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = dir.Close() }()
	file = FileName(f.now(), f.random(), name)
	// O_EXCL makes the file where no entry is: os.Root never follows a
	// link for it, whatever an agent planted at the name.
	out, err := dir.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	size, err = io.Copy(out, body)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = dir.Remove(file)
		return "", 0, err
	}
	return file, size, nil
}

// Remove removes session's file, as for an upload that could not be
// recorded. A file that is gone is no error.
func (f *Folder) Remove(session int64, file string) error {
	if !oneName(file) {
		return fmt.Errorf("%q is no upload's file name", file)
	}
	dir, err := f.open(session, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ErrGrown is a file larger than the upload it was.
var ErrGrown = errors.New("the file is larger than it was uploaded")

// ErrNotRegular is a name in the uploads folder that holds anything but a
// regular file.
var ErrNotRegular = errors.New("not a regular file")

// Read reads session's file, at most max bytes (ErrGrown for more): only
// a regular file inside the session's folder, through no link that leaves
// it, and without waiting on a named pipe.
func (f *Folder) Read(session int64, file string, max int64) ([]byte, error) {
	if !oneName(file) {
		return nil, fmt.Errorf("%q is no upload's file name", file)
	}
	dir, err := f.open(session, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	in, err := dir.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(in, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrGrown
	}
	return data, nil
}

// oneName says file is one name in a folder, as Store makes one.
func oneName(file string) bool {
	return file != "" && file != "." && file != ".." && !strings.ContainsAny(file, "/\x00")
}
