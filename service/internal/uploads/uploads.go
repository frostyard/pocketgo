// Package uploads keeps the files the owner attaches to messages
// (ADR-0029): in the service user's ~/.local/share/pocketgo-uploads, one
// folder a session, outside pocketd's data folder, where the agents'
// sandbox lets them read the files, and change none (ADR-0035).
//
// The agents run as pocketd's own user, could write there before ADR-0035,
// and the owner still can, so pocketd follows no link in that folder, and
// never waits on what was put there: it opens each folder below ~/.local/share by its descriptor, as a
// folder and not through a link, writes a new file unnamed and gives it
// its name only once it is whole and where no entry is, and reads a file
// only as the regular file its name holds. It removes nothing there: an
// upload a stop of pocketd cut off is finished, or left as the agents left
// it (Codex review of #49, round 3).
package uploads

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
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
	// staging is a folder the agents cannot reach, in pocketd's data
	// folder: a file is written there, and named in the uploads folder
	// once whole, where the host makes no unnamed file ("" for none). run
	// begins the names this run of pocketd stages, so a later run tells
	// what a stop left there.
	staging, run string
	// now and random name each new file (FileName); tests set them, and
	// noUnnamed, which stages as a host without unnamed files does.
	now       func() time.Time
	random    func() string
	noUnnamed bool
}

// New is the uploads folder at base, its new files named at now's time,
// staged, where the host makes no unnamed file, in staging.
func New(base, staging string, now func() time.Time) *Folder {
	if now == nil {
		now = time.Now
	}
	f := &Folder{base: filepath.Clean(base), now: now, random: randomHex}
	if staging != "" {
		f.staging = filepath.Clean(staging)
	}
	f.run = f.random() + f.random() // 16 hexadecimal characters
	return f
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
// once, as pasted images do, never meet. A safe name is its own safe name,
// so the file ends in "-" and the name its record keeps (Codex review of
// #49, round 1).
func FileName(at time.Time, random, name string) string {
	return strconv.FormatInt(at.UnixMilli(), 36) + "-" + random + "-" + SafeName(name)
}

// NewFile is the name of a new file uploaded as name (FileName), at the
// folder's time.
func (f *Folder) NewFile(name string) string { return FileName(f.now(), f.random(), name) }

// SafeName is name as pocketd keeps it (proto.SafeUploadName).
func SafeName(name string) string { return proto.SafeUploadName(name) }

// Mime is an upload's type (proto.UploadMime).
func Mime(name, declared string) string { return proto.UploadMime(name, declared) }

// ErrNotItsFolder is a folder of the uploads folder whose name holds a
// link, or anything but a folder: pocketd neither follows nor opens it.
var ErrNotItsFolder = errors.New("a link or another entry stands where the uploads folder has a folder")

// ErrNotOurs is a folder of the uploads folder another user owns.
var ErrNotOurs = errors.New("the folder is not the service user's")

// ErrNameTaken is a new file's name that an entry holds already: Link made
// nothing, and the entry stays as it is.
var ErrNameTaken = errors.New("an entry holds the new file's name")

// ErrGrown is a file larger than the upload it was.
var ErrGrown = errors.New("the file is larger than it was uploaded")

// ErrChanged is a file whose bytes are not those it was uploaded with.
var ErrChanged = errors.New("the file changed since it was uploaded")

// ReadUpload reads session's file as Read does, and only as the bytes it
// was uploaded with: size bytes whose SHA-256 is digest, in hexadecimal,
// as Stage kept them; else ErrChanged. A file the agents changed before
// the uploads folder was read-only to them (ADR-0035), through it or
// through another name a hard link gave it, or that the owner changed by
// hand, is never given to a model as the upload it was (Codex review of
// #63, round 2).
func (f *Folder) ReadUpload(session int64, file string, size int64, digest string) ([]byte, error) {
	data, err := f.Read(session, file, size)
	if errors.Is(err, ErrGrown) {
		// Bytes added to it change it too (Codex review of #63, round 6).
		return nil, ErrChanged
	} else if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || digest == "" || hex.EncodeToString(sum[:]) != digest {
		return nil, ErrChanged
	}
	return data, nil
}

// ErrNotSynced is a file Link named whose name is not yet on disk in its
// folder: the upload is named but does not count until a later sync, as
// Holds makes, writes it (Codex review of #49, round 6).
var ErrNotSynced = errors.New("the file is named, but its name is not yet on disk")

// ErrStagingElsewhere is an upload that can go nowhere: the uploads
// folder's file system makes no unnamed file, and the staging folder is
// on another file system, so the staged file cannot be named there. It is
// refused before its body is read (Codex review of #49, round 6).
var ErrStagingElsewhere = errors.New("the uploads folder's file system makes no unnamed file, and pocketd's data folder, which holds the staging folder, is on another file system: put the data folder on the file system of ~/.local/share")

// ErrNotRegular is a name in the uploads folder that holds anything but a
// regular file: a link, even one to another file of the folder, included.
var ErrNotRegular = errors.New("not a regular file")

// Identity is what tells a file pocketd made from any other, kept with
// its upload before it is named: its device and its inode, its size, and
// its bytes' SHA-256 in hexadecimal, since an inode is used again once a
// file is gone (Codex review of #49, round 4).
type Identity struct {
	Dev, Ino uint64
	Size     int64
	Digest   string
}

// Store keeps body as session's new upload, named after name
// (FileName), and says its file's name and size: Stage, then Link.
func (f *Folder) Store(session int64, name string, body io.Reader) (file string, size int64, err error) {
	staged, err := f.Stage(session, body)
	if err != nil {
		return "", 0, err
	}
	defer staged.Close()
	file = f.NewFile(name)
	if err := staged.Link(file); err != nil {
		return "", 0, err
	}
	return file, staged.Identity.Size, nil
}

// oneName says file is one name in a folder, as Store makes one.
func oneName(file string) bool {
	return file != "" && file != "." && file != ".." && !strings.ContainsAny(file, "/\x00")
}
