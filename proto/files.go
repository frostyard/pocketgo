package proto

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The file viewer's limits (ADR-0066, protocol rules 70 to 72).
const (
	MaxFilePath      = 4096    // most bytes of a path the viewer takes
	MaxFolderEntries = 1000    // most entries one folder's listing gives
	MaxFolderNames   = 10000   // most names pocketd reads of one folder
	MaxFileText      = 1 << 20 // most bytes of a text file it sends
	MaxFileImage     = 5 << 20 // most bytes of an image file it reads
	MaxFileReads     = 4       // reads that run at once
)

// withheldParts are fragments that suggest a secret.
var withheldParts = []string{"token", "secret", "password", "passwd", "credential", "apikey", "api_key", "api-key"}

// withheldSuffixes are secret file extensions.
var withheldSuffixes = []string{".key", ".pem", ".p12", ".pfx", ".jks", ".keystore", ".kdbx", ".gpg", ".env"}

// withheldPrefixes are private-key name prefixes.
var withheldPrefixes = []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

// withheldNames are known credential file names.
var withheldNames = []string{"auth.json", "netrc", "_netrc"}

// CheckFilePath cleans a path below an agent's home and refuses one the viewer never reads.
func CheckFilePath(p string) (string, error) {
	if len(p) > MaxFilePath || !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("path must be at most %d bytes of UTF-8 without a NUL", MaxFilePath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path must be a path from the agent's home, not an absolute path")
	}
	c := path.Clean(p)
	if p == "" || c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path leaves the agent's home")
	}
	for _, part := range strings.Split(c, "/") {
		if HiddenName(part) {
			return "", fmt.Errorf("%q: names that start with a dot are hidden", clipPath(part))
		}
		if WithheldName(part) {
			return "", fmt.Errorf("%q: its name looks like it holds a secret, so it is withheld", clipPath(part))
		}
	}
	return c, nil
}

// HiddenName says the viewer neither lists nor reads an entry of this name.
func HiddenName(name string) bool { return strings.HasPrefix(name, ".") }

// WithheldName says the viewer lists an entry of this name but never opens or reads it: its name looks like it holds a secret.
func WithheldName(name string) bool {
	n := strings.ToLower(name)
	for _, s := range withheldParts {
		if strings.Contains(n, s) {
			return true
		}
	}
	for _, s := range withheldSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	for _, s := range withheldPrefixes {
		if strings.HasPrefix(n, s) {
			return true
		}
	}
	for _, s := range withheldNames {
		if n == s {
			return true
		}
	}
	return false
}

// Paths of the file viewer (ADR-0066, rules 70 to 72); list and read take ?root=<id>&path=<path>.
const (
	PathFiles     = "/v1/files"
	PathFilesList = "/v1/files/list"
	PathFilesRead = "/v1/files/read"
)

// SessionFilesRoot names the session's recorded working directory in list and read requests.
func SessionFilesRoot(id int64) string { return "session:" + strconv.FormatInt(id, 10) }

// ParseSessionFilesRoot accepts only the canonical form of a positive session ID.
func ParseSessionFilesRoot(root string) (int64, bool) {
	raw, ok := strings.CutPrefix(root, "session:")
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, ok && err == nil && id > 0 && root == SessionFilesRoot(id)
}

// Kinds of a FileEntry.
const (
	FileEntryFolder = "folder"
	FileEntryFile   = "file"
	FileEntryOther  = "other"
)

// Kinds of a FileContent.
const (
	FileMarkdown = "markdown" // Text holds it, to draw as Markdown
	FileText     = "text"     // Text holds it, to draw as plain monospace text
	FileImage    = "image"    // Image and MimeType hold it
	FileBinary   = "binary"   // not shown; Note says why
	FileLarge    = "large"    // over its limit; Note says which
)

// FileRoots lists the profiles' readable homes.
type FileRoots struct {
	// SessionFolders advertises session-scoped list and read on this service.
	SessionFolders bool       `json:"sessionFolders,omitempty"`
	Home           string     `json:"home,omitempty"` // the service user's home, which "~/" stands for
	Roots          []FileRoot `json:"roots"`          // by id; never nil
}

// FileRoot names an agent's home.
type FileRoot struct {
	ID      string `json:"id"`   // the profile's id
	Name    string `json:"name"` // its name, or its id
	Path    string `json:"path"` // the agent's home, absolute
	Private bool   `json:"private,omitempty"`
}

// FileFolder is a bounded listing below one home.
type FileFolder struct {
	Root    string      `json:"root"`
	Path    string      `json:"path"`    // as asked, cleaned; empty for the home
	Real    string      `json:"real"`    // real path below the home, links resolved
	Entries []FileEntry `json:"entries"` // never nil
	More    int         `json:"more,omitempty"`
	Partial bool        `json:"partial,omitempty"`
}

// FileEntry describes an entry without reading its contents.
type FileEntry struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Link     bool      `json:"link,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Modified time.Time `json:"modified"`
	Withheld bool      `json:"withheld,omitempty"`
}

// FileContent is a file prepared for read-only display.
type FileContent struct {
	Root     string    `json:"root"`
	Path     string    `json:"path"`
	Real     string    `json:"real"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Kind     string    `json:"kind"`
	Text     string    `json:"text,omitempty"`
	Image    []byte    `json:"image,omitempty"`
	MimeType string    `json:"mimeType,omitempty"`
	Note     string    `json:"note,omitempty"`
}
