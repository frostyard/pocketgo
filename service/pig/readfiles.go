package pig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/service/internal/files"
)

// ReadFilesHelper is the hidden pocketd command Engine.ListFiles and
// Engine.ReadFile run in the sandbox to read below an agent's home (ADR-0066):
//
// ReadFilesHelper list ROOT REL MAXENTRIES MAXNAMES
// ReadFilesHelper read ROOT REL MAX
//
// REL is "." for the home itself.
const ReadFilesHelper = "__read-files"

// filesHead is one helper answer, followed only by a read's raw bytes.
type filesHead struct {
	Error   string        `json:"error,omitempty"`
	Message string        `json:"message,omitempty"`
	Folder  *files.Folder `json:"folder,omitempty"`
	File    *filesMeta    `json:"file,omitempty"`
}

// filesMeta is a file's metadata, without its contents in the JSON line.
type filesMeta struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Over     bool      `json:"over"`
}

// RunReadFilesHelper writes a bounded listing or read, with known errors in JSON.
func RunReadFilesHelper(args []string, w io.Writer) int {
	if len(args) < 4 || args[1] == "" || !filepath.IsAbs(args[1]) {
		return 2
	}
	var head filesHead
	var data []byte
	var err error
	switch args[0] {
	case "list":
		if len(args) != 5 {
			return 2
		}
		entries, e1 := strconv.Atoi(args[3])
		names, e2 := strconv.Atoi(args[4])
		if e1 != nil || e2 != nil || entries < 0 || names < 0 {
			return 2
		}
		var folder files.Folder
		folder, err = files.List(args[1], args[2], entries, names)
		if err == nil {
			head.Folder = &folder
		}
	case "read":
		if len(args) != 4 {
			return 2
		}
		max, perr := strconv.ParseInt(args[3], 10, 64)
		if perr != nil || max < 0 {
			return 2
		}
		var file files.File
		file, err = files.Read(args[1], args[2], max)
		if err == nil {
			head.File = &filesMeta{Path: file.Path, Size: file.Size, Modified: file.Modified, Over: file.Over}
			data = file.Data
		}
	default:
		return 2
	}
	if err != nil {
		code := files.Code(err)
		if code == "" {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		head = filesHead{Error: code, Message: err.Error()}
	}
	if err := json.NewEncoder(w).Encode(head); err != nil {
		return 1
	}
	if _, err := w.Write(data); err != nil {
		return 1
	}
	return 0
}

// parseFilesAnswer checks the head and its verb before exposing any bytes.
func parseFilesAnswer(verb, out string) (files.Folder, files.File, error) {
	none := fmt.Errorf("read files: no answer")
	line, data, ok := strings.Cut(out, "\n")
	if !ok {
		return files.Folder{}, files.File{}, none
	}
	var head filesHead
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&head); err != nil {
		return files.Folder{}, files.File{}, none
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return files.Folder{}, files.File{}, none
	}
	count := 0
	if head.Error != "" {
		count++
	}
	if head.Folder != nil {
		count++
	}
	if head.File != nil {
		count++
	}
	if count != 1 {
		return files.Folder{}, files.File{}, none
	}
	if head.Error != "" {
		err, ok := files.FromCode(head.Error, head.Message)
		if !ok || data != "" {
			return files.Folder{}, files.File{}, none
		}
		return files.Folder{}, files.File{}, err
	}
	if head.Message != "" {
		return files.Folder{}, files.File{}, none
	}
	switch verb {
	case "list":
		if head.Folder == nil || data != "" {
			return files.Folder{}, files.File{}, none
		}
		folder := *head.Folder
		if folder.Entries == nil {
			folder.Entries = []files.Entry{}
		}
		return folder, files.File{}, nil
	case "read":
		if head.File == nil {
			return files.Folder{}, files.File{}, none
		}
		meta := head.File
		if meta.Size < 0 || (meta.Over && data != "") || (!meta.Over && int64(len(data)) != meta.Size) {
			return files.Folder{}, files.File{}, none
		}
		file := files.File{Path: meta.Path, Size: meta.Size, Modified: meta.Modified, Over: meta.Over}
		if !meta.Over {
			file.Data = []byte(data)
		}
		return files.Folder{}, file, nil
	}
	return files.Folder{}, files.File{}, none
}

// ListFiles lists a home through ReadFilesHelper inside the sandbox.
func (e *Engine) ListFiles(ctx context.Context, root, rel string, maxEntries, maxNames int) (files.Folder, error) {
	if !filepath.IsAbs(root) || maxEntries < 0 || maxNames < 0 {
		return files.Folder{}, fmt.Errorf("read files: invalid root or bounds")
	}
	if rel == "" {
		rel = "."
	}
	out, err := e.runHelper(ctx, 8<<20, ReadFilesHelper, "list", root, rel, strconv.Itoa(maxEntries), strconv.Itoa(maxNames))
	if err != nil {
		return files.Folder{}, err
	}
	folder, _, err := parseFilesAnswer("list", out)
	return folder, err
}

// ReadFile reads a home file through ReadFilesHelper inside the sandbox.
func (e *Engine) ReadFile(ctx context.Context, root, rel string, max int64) (files.File, error) {
	// Leave room for metadata and an extra byte without overflowing int.
	if !filepath.IsAbs(root) || max < 0 || uint64(max) > uint64(^uint(0)>>1)-(64<<10)-1 {
		return files.File{}, fmt.Errorf("read files: invalid root or bounds")
	}
	if rel == "" {
		rel = "."
	}
	out, err := e.runHelper(ctx, (64<<10)+int(max)+1, ReadFilesHelper, "read", root, rel, strconv.FormatInt(max, 10))
	if err != nil {
		return files.File{}, err
	}
	_, file, err := parseFilesAnswer("read", out)
	return file, err
}
