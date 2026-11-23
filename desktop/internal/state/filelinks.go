package state

import (
	"context"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/frostyard/pocketgo/proto"
)

// FileLinkScheme starts a destination that opens the Files view.
const FileLinkScheme = "pocketgo-file:"

// FileLinkDest encodes a root and every part of its relative path separately.
func FileLinkDest(root, p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return FileLinkScheme + url.PathEscape(root) + "/" + strings.Join(parts, "/")
}

// ParseFileLink accepts this scheme, a nonempty root and a readable relative path.
func ParseFileLink(dest string) (root, p string, ok bool) {
	rest, ok := strings.CutPrefix(dest, FileLinkScheme)
	if !ok {
		return "", "", false
	}
	encodedRoot, encodedPath, found := strings.Cut(rest, "/")
	if !found {
		return "", "", false
	}
	root, err := url.PathUnescape(encodedRoot)
	if err != nil || root == "" {
		return "", "", false
	}
	p, err = url.PathUnescape(encodedPath)
	if err != nil {
		return "", "", false
	}
	p, err = proto.CheckFilePath(p)
	if err != nil {
		return "", "", false
	}
	return root, p, true
}

// absoluteFileSpot matches a cleaned absolute path to its longest containing home.
func absoluteFileSpot(roots proto.FileRoots, p string) (root, rel string, ok bool) {
	if strings.HasPrefix(p, "~/") {
		if roots.Home == "" {
			return "", "", false
		}
		p = roots.Home + p[1:]
	}
	if !strings.HasPrefix(p, "/") {
		return "", "", false
	}
	p = path.Clean(p)
	best := -1
	for _, r := range roots.Roots {
		if r.ID == "" || !strings.HasPrefix(r.Path, "/") {
			continue
		}
		home := path.Clean(r.Path)
		prefix := strings.TrimSuffix(home, "/") + "/"
		if (p == home || strings.HasPrefix(p, prefix)) && len(home) > best {
			root = r.ID
			rel = strings.TrimPrefix(p, prefix)
			if p == home {
				rel = ""
			}
			best = len(home)
		}
	}
	if best < 0 {
		return "", "", false
	}
	clean, err := proto.CheckFilePath(rel)
	if err != nil {
		return "", "", false
	}
	return root, clean, true
}

// FileLink links inline code only when it names a known path below a readable home.
func FileLink(code string, roots proto.FileRoots, openRoot string, top []string) (string, bool) {
	if code == "" || len(code) > proto.MaxFilePath || strings.ContainsFunc(code, unicode.IsSpace) {
		return "", false
	}
	if strings.HasPrefix(code, "/") || strings.HasPrefix(code, "~/") {
		root, p, ok := absoluteFileSpot(roots, code)
		if !ok {
			return "", false
		}
		return FileLinkDest(root, p), true
	}
	if strings.HasPrefix(code, "~") || strings.Contains(code, "://") || openRoot == "" || !slices.ContainsFunc(roots.Roots, func(r proto.FileRoot) bool { return r.ID == openRoot }) {
		return "", false
	}
	p, err := proto.CheckFilePath(code)
	if err != nil || p == "" {
		return "", false
	}
	first, _, _ := strings.Cut(p, "/")
	if !slices.Contains(top, first) {
		return "", false
	}
	return FileLinkDest(openRoot, p), true
}

// OpenFileRoot is the open session's profile when the homes list contains it.
func (d *Data) OpenFileRoot() string {
	session, ok := d.Session(d.OpenID())
	if !ok || session.Profile == "" {
		return ""
	}
	for _, r := range d.fileRoots.Roots.Roots {
		if r.ID == session.Profile {
			return r.ID
		}
	}
	return ""
}

// fileTop is a home's last successful top listing and its newest answered fetch.
type fileTop struct {
	names []string
	known bool
	list  list
}

// FileTop is the names of a home's last successful top listing, if any.
func (d *Data) FileTop(root string) ([]string, bool) {
	top := d.fileTops[root]
	if top == nil || !top.known {
		return nil, false
	}
	return top.names, true
}

// setFileTop records a successful top listing unless a later fetch already answered.
func (d *Data) setFileTop(f Fetch, root string, folder proto.FileFolder) {
	top := d.topFor(root)
	if !top.list.answer(f) {
		return
	}
	d.rev++
	top.names = make([]string, 0, len(folder.Entries))
	for _, entry := range folder.Entries {
		top.names = append(top.names, entry.Name)
	}
	top.known = true
}

// topFor makes a home's fetch record without claiming to know its names.
func (d *Data) topFor(root string) *fileTop {
	if d.fileTops == nil {
		d.fileTops = map[string]*fileTop{}
	}
	if d.fileTops[root] == nil {
		d.rev++
		d.fileTops[root] = &fileTop{}
	}
	return d.fileTops[root]
}

// setFileTopError retires a failed fetch while preserving any successful names.
func (d *Data) setFileTopError(f Fetch, root string) {
	if d.topFor(root).list.answer(f) {
		d.rev++
	}
}

// FileLinkOf links inline code with this session's home and its known top names.
func (d *Data) FileLinkOf(code string) (string, bool) {
	root := d.OpenFileRoot()
	names, _ := d.FileTop(root)
	return FileLink(code, d.fileRoots.Roots, root, names)
}

// ResolveFileHref resolves a Markdown link relative to the file being shown.
func ResolveFileHref(roots proto.FileRoots, spot FileSpot, href string) (root, p string, ok bool) {
	if !spot.File {
		return "", "", false
	}
	if strings.HasPrefix(href, FileLinkScheme) {
		return ParseFileLink(href)
	}
	if href == "" || strings.HasPrefix(href, "#") {
		return "", "", false
	}
	if colon := strings.IndexByte(href, ':'); colon >= 0 {
		first := strings.IndexAny(href, "/?#")
		if first < 0 || colon < first {
			return "", "", false
		}
	}
	if cut := strings.IndexAny(href, "?#"); cut >= 0 {
		href = href[:cut]
	}
	decoded, err := url.PathUnescape(href)
	if err != nil {
		return "", "", false
	}
	if strings.HasPrefix(decoded, "/") || strings.HasPrefix(decoded, "~/") {
		return absoluteFileSpot(roots, decoded)
	}
	if spot.Root == "" {
		return "", "", false
	}
	p, err = proto.CheckFilePath(JoinFilePath(ParentPath(spot.Path), decoded))
	if err != nil {
		return "", "", false
	}
	return spot.Root, p, true
}

// LoadFileTop fetches names for linking, never changing the Files view's selections.
func (s *Store) LoadFileTop(root string) {
	if root == "" {
		return
	}
	f := s.Data.Begin()
	var folder proto.FileFolder
	s.do("Loading the home folder's names", func(ctx context.Context, c *proto.Client) (err error) {
		folder, err = c.ListFiles(ctx, root, "")
		return err
	}, s.track("files:top:"+root, func(err error) {
		if err != nil {
			s.Data.setFileTopError(f, root)
		} else {
			s.Data.setFileTop(f, root, folder)
		}
	}))
}

// syncFileTop fetches the open agent's top names when none are known or in flight.
func (s *Store) syncFileTop() {
	root := s.Data.OpenFileRoot()
	if root == "" {
		return
	}
	if _, known := s.Data.FileTop(root); !known && s.fetching["files:top:"+root] == 0 {
		s.LoadFileTop(root)
	}
}
