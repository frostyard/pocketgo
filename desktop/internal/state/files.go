package state

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// PlaceFiles names the read-only Files view.
const PlaceFiles = "files"

// FileSpot is a place in Files: no Root means homes; File selects a file rather than a folder.
type FileSpot struct {
	Root, Path string
	File       bool
}

// FileRootsView is what the client knows of the agents' homes.
type FileRootsView struct {
	Status ListStatus
	Err    string
	Roots  proto.FileRoots
}

// ShownFolder is the spot's folder, or the folder beside its file.
type ShownFolder struct {
	Root, Path string
	Status     ListStatus
	Err        string
	Folder     proto.FileFolder
	list       list
}

// ShownFile is the file the Files view shows.
type ShownFile struct {
	Root, Path string
	Status     ListStatus
	Err        string
	Content    proto.FileContent
	list       list
}

// FileRoots is the homes' last answer and fetch status.
func (d *Data) FileRoots() FileRootsView { return d.fileRoots }

// BeginFileRoots starts a fetch without discarding earlier homes.
func (d *Data) BeginFileRoots() Fetch { d.rev++; return d.Begin() }

// SetFileRoots applies the homes unless a later-begun fetch already answered.
func (d *Data) SetFileRoots(f Fetch, r proto.FileRoots) {
	if !d.fileRootsList.answer(f) {
		return
	}
	d.rev++
	r.Roots = slices.Clone(r.Roots)
	if r.Roots == nil {
		r.Roots = []proto.FileRoot{}
	}
	d.fileRoots = FileRootsView{Status: ListLoaded, Roots: r}
}

// SetFileRootsError keeps the homes an earlier answer brought.
func (d *Data) SetFileRootsError(f Fetch, err string) {
	if !d.fileRootsList.answer(f) {
		return
	}
	d.rev++
	d.fileRoots.Status, d.fileRoots.Err = ListFailed, err
}

// FilesFolder is the selected folder, if any.
func (d *Data) FilesFolder() (ShownFolder, bool) {
	if d.filesFolder == nil {
		return ShownFolder{}, false
	}
	return *d.filesFolder, true
}

// BeginFolder shows a folder, keeping its own earlier listing but no other folder's.
func (d *Data) BeginFolder(root, p string) Fetch {
	f := d.Begin()
	d.rev++
	if d.filesFolder == nil || d.filesFolder.Root != root || d.filesFolder.Path != p {
		d.filesFolder = &ShownFolder{Root: root, Path: p, list: list{applied: f.epoch}}
	}
	return f
}

// SetFolder applies a still-shown folder's answer unless a later fetch answered.
func (d *Data) SetFolder(f Fetch, root, p string, folder proto.FileFolder) {
	shown := d.filesFolder
	if shown == nil || shown.Root != root || shown.Path != p || !shown.list.answer(f) {
		return
	}
	d.rev++
	folder.Entries = slices.Clone(folder.Entries)
	if folder.Entries == nil {
		folder.Entries = []proto.FileEntry{}
	}
	shown.Status, shown.Err, shown.Folder = ListLoaded, "", folder
	if p == "" {
		d.setFileTop(f, root, folder)
	}
}

// SetFolderError records a failure while keeping the earlier listing.
func (d *Data) SetFolderError(f Fetch, root, p, err string) {
	shown := d.filesFolder
	if shown == nil || shown.Root != root || shown.Path != p || !shown.list.answer(f) {
		return
	}
	d.rev++
	shown.Status, shown.Err = ListFailed, err
}

// FilesFile is the selected file, if any.
func (d *Data) FilesFile() (ShownFile, bool) {
	if d.filesFile == nil {
		return ShownFile{}, false
	}
	return *d.filesFile, true
}

// BeginFile shows a file, keeping its own earlier contents but no other file's.
func (d *Data) BeginFile(root, p string) Fetch {
	f := d.Begin()
	d.rev++
	if d.filesFile == nil || d.filesFile.Root != root || d.filesFile.Path != p {
		d.filesFile = &ShownFile{Root: root, Path: p, list: list{applied: f.epoch}}
	}
	return f
}

// SetFile applies a still-shown file's answer unless a later fetch answered.
func (d *Data) SetFile(f Fetch, root, p string, c proto.FileContent) {
	shown := d.filesFile
	if shown == nil || shown.Root != root || shown.Path != p || !shown.list.answer(f) {
		return
	}
	d.rev++
	c.Image = slices.Clone(c.Image)
	shown.Status, shown.Err, shown.Content = ListLoaded, "", c
}

// SetFileError records a failure while keeping the earlier contents.
func (d *Data) SetFileError(f Fetch, root, p, err string) {
	shown := d.filesFile
	if shown == nil || shown.Root != root || shown.Path != p || !shown.list.answer(f) {
		return
	}
	d.rev++
	shown.Status, shown.Err = ListFailed, err
}

// FilesSpot is the file shown, else its folder, else the homes.
func (d *Data) FilesSpot() FileSpot {
	if f := d.filesFile; f != nil {
		return FileSpot{Root: f.Root, Path: f.Path, File: true}
	}
	if f := d.filesFolder; f != nil {
		return FileSpot{Root: f.Root, Path: f.Path}
	}
	return FileSpot{}
}

// FilesNote is the explanation shown above the folder.
func (d *Data) FilesNote() string { return d.filesNote }

// setFilesNote changes the explanation of a path that cannot open.
func (d *Data) setFilesNote(note string) { d.rev++; d.filesNote = note }

// closeFile forgets the selected file.
func (d *Data) closeFile() {
	if d.filesFile != nil {
		d.rev++
		d.filesFile = nil
	}
}

// closeFiles forgets both selections and their explanation.
func (d *Data) closeFiles() { d.rev++; d.filesFolder = nil; d.filesFile = nil; d.filesNote = "" }

// ParentPath is the folder above p, empty for the home's top.
func ParentPath(p string) string {
	if p == "" {
		return ""
	}
	parent := path.Dir(p)
	if parent == "." {
		return ""
	}
	return parent
}

// JoinFilePath appends a listed name to a folder's relative path.
func JoinFilePath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// Crumb is one named navigation step.
type Crumb struct {
	Label string
	Spot  FileSpot
}

// FileCrumbs names the homes, selected home and every part of the path.
func FileCrumbs(roots proto.FileRoots, spot FileSpot) []Crumb {
	out := []Crumb{{Label: "Homes"}}
	if spot.Root == "" {
		return out
	}
	name := spot.Root
	for _, r := range roots.Roots {
		if r.ID == spot.Root {
			name = r.Name
			if name == "" {
				name = r.ID
			}
			break
		}
	}
	out = append(out, Crumb{Label: name, Spot: FileSpot{Root: spot.Root}})
	if spot.Path == "" {
		return out
	}
	parts := strings.Split(spot.Path, "/")
	p := ""
	for i, part := range parts {
		p = JoinFilePath(p, part)
		out = append(out, Crumb{Label: part, Spot: FileSpot{Root: spot.Root, Path: p, File: spot.File && i == len(parts)-1}})
	}
	return out
}

// SortFileEntries copies the listing with folders first, stable within each group.
func SortFileEntries(entries []proto.FileEntry) []proto.FileEntry {
	out := slices.Clone(entries)
	slices.SortStableFunc(out, func(a, b proto.FileEntry) int {
		if (a.Kind == proto.FileEntryFolder) == (b.Kind == proto.FileEntryFolder) {
			return 0
		}
		if a.Kind == proto.FileEntryFolder {
			return -1
		}
		return 1
	})
	return out
}

// EntryOpens says pocketd offers this entry as a readable file or folder.
func EntryOpens(e proto.FileEntry) bool {
	return !e.Withheld && (e.Kind == proto.FileEntryFolder || e.Kind == proto.FileEntryFile)
}

// FileSize formats nonnegative byte counts in binary units.
func FileSize(n int64) string {
	switch {
	case n < 0:
		return ""
	case n == 1:
		return "1 byte"
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	}
	size := float64(n) / 1024
	unit := "KiB"
	if n >= 1<<30 {
		size = float64(n) / (1 << 30)
		unit = "GiB"
	} else if n >= 1<<20 {
		size = float64(n) / (1 << 20)
		unit = "MiB"
	}
	return fmt.Sprintf("%.1f %s", size, unit)
}

// EntryCaption names an entry's kind, size or reason it is withheld.
func EntryCaption(e proto.FileEntry) string {
	var caption string
	switch {
	case e.Withheld:
		caption = "Withheld: its name looks like it holds a secret"
	case e.Kind == proto.FileEntryFolder:
		caption = "Folder"
	case e.Kind == proto.FileEntryFile:
		caption = FileSize(e.Size)
	default:
		caption = "Not shown"
	}
	if e.Link {
		caption += ", a link"
	}
	return caption
}

// fileCount formats a positive count with thousands separators.
func fileCount(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// FolderNotes says what the bounded listing left out.
func FolderNotes(f proto.FileFolder) []string {
	var notes []string
	if f.More == 1 {
		notes = append(notes, "1 more entry is not listed.")
	} else if f.More > 1 {
		notes = append(notes, fmt.Sprintf("%d more entries are not listed.", f.More))
	}
	if f.Partial {
		n := fileCount(proto.MaxFolderNames)
		notes = append(notes, "This folder holds more than "+n+" names: these are from the first "+n+" read.")
	}
	return notes
}

// FileShows is the drawable content kind, empty for content never shown.
func FileShows(c proto.FileContent) string {
	switch c.Kind {
	case proto.FileMarkdown, proto.FileText, proto.FileImage:
		return c.Kind
	}
	return ""
}

// FileNote explains withheld content or an image's preparation.
func FileNote(c proto.FileContent) string {
	switch c.Kind {
	case proto.FileBinary, proto.FileLarge:
		if c.Note == "" {
			return "Not shown."
		}
	case proto.FileImage:
	default:
		return ""
	}
	if c.Note == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(c.Note)
	return string(unicode.ToUpper(r)) + c.Note[n:]
}

// LoadFileRoots fetches homes; the newest answered fetch wins.
func (s *Store) LoadFileRoots() {
	f := s.Data.BeginFileRoots()
	var roots proto.FileRoots
	s.do("Loading the agents' homes", func(ctx context.Context, c *proto.Client) (err error) {
		roots, err = c.FileRoots(ctx)
		return err
	}, s.track("fileroots", func(err error) {
		if err != nil {
			s.Data.SetFileRootsError(f, err.Error())
		} else {
			s.Data.SetFileRoots(f, roots)
			if s.Data.fileRootsList.applied == f.epoch {
				s.syncFileTop()
			}
		}
	}))
}

// loadFolder fetches one folder, optionally following an unknown-kind path once applied.
func (s *Store) loadFolder(root, p string, applied func(proto.FileFolder)) {
	f := s.Data.BeginFolder(root, p)
	var folder proto.FileFolder
	s.do("Loading the folder", func(ctx context.Context, c *proto.Client) (err error) {
		folder, err = c.ListFiles(ctx, root, p)
		return err
	}, s.track("files:folder", func(err error) {
		if err != nil {
			s.Data.SetFolderError(f, root, p, err.Error())
			return
		}
		s.Data.SetFolder(f, root, p, folder)
		shown, ok := s.Data.FilesFolder()
		if applied != nil && ok && shown.Root == root && shown.Path == p && shown.list.applied == f.epoch {
			applied(folder)
		}
	}))
}

// loadFile fetches one file while keeping earlier content of that file.
func (s *Store) loadFile(root, p string) {
	f := s.Data.BeginFile(root, p)
	var content proto.FileContent
	s.do("Loading the file", func(ctx context.Context, c *proto.Client) (err error) {
		content, err = c.ReadFile(ctx, root, p)
		return err
	}, s.track("files:file", func(err error) {
		if err != nil {
			s.Data.SetFileError(f, root, p, err.Error())
		} else {
			s.Data.SetFile(f, root, p, content)
		}
	}))
}

// OpenFiles navigates to homes, a folder, or a file with its containing folder.
func (s *Store) OpenFiles(spot FileSpot) {
	s.filesNav++
	s.Data.setFilesNote("")
	if spot.Root == "" {
		s.Data.closeFiles()
		s.LoadFileRoots()
		return
	}
	if !spot.File {
		s.Data.closeFile()
		s.loadFolder(spot.Root, spot.Path, nil)
		return
	}
	s.loadFile(spot.Root, spot.Path)
	parent := ParentPath(spot.Path)
	if f, ok := s.Data.FilesFolder(); !ok || f.Root != spot.Root || f.Path != parent {
		s.loadFolder(spot.Root, parent, nil)
	}
}

// OpenFilePath looks up an unknown-kind link in its parent's fresh listing.
func (s *Store) OpenFilePath(root, p string) {
	if p == "" {
		s.OpenFiles(FileSpot{Root: root})
		return
	}
	s.filesNav++
	nav := s.filesNav
	s.Data.closeFile()
	s.Data.setFilesNote("")
	name := path.Base(p)
	s.loadFolder(root, ParentPath(p), func(folder proto.FileFolder) {
		// A navigation since the lookup began wins: the owner chose
		// another place while the folder was read.
		if s.filesNav != nav {
			return
		}
		for _, e := range folder.Entries {
			if e.Name != name {
				continue
			}
			switch {
			case EntryOpens(e):
				s.OpenFiles(FileSpot{Root: root, Path: p, File: e.Kind == proto.FileEntryFile})
			case e.Withheld:
				s.Data.setFilesNote(name + " is withheld: its name looks like it holds a secret.")
			default:
				s.Data.setFilesNote(name + " cannot be shown.")
			}
			return
		}
		note := name + " is not in this folder."
		if folder.More > 0 || folder.Partial {
			note += " It may be past the entries listed."
		}
		s.Data.setFilesNote(note)
	})
}

// RefreshFiles fetches the homes and both shown selections without navigating.
func (s *Store) RefreshFiles() {
	s.LoadFileRoots()
	if f, ok := s.Data.FilesFolder(); ok {
		s.loadFolder(f.Root, f.Path, nil)
	}
	if f, ok := s.Data.FilesFile(); ok {
		s.loadFile(f.Root, f.Path)
	}
}
