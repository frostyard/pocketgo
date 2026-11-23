package files

import (
	"path/filepath"
	"sort"
	"strings"
)

// Home is an agent's home the viewer may read: a profile's.
type Home struct {
	ID, Name, Path string
	Private        bool
}

// Roots are the homes the viewer reads, by ID. Absolute homes are kept
// only when they neither equal, hold nor lie inside a refused path,
// comparing both paths as written and, where possible, with links resolved.
func Roots(homes []Home, refused []string) []Home {
	blocked := []string{}
	for _, p := range refused {
		if p != "" {
			blocked = append(blocked, pathForms(p)...)
		}
	}
	out := []Home{}
	for _, h := range homes {
		if h.Path == "" || !filepath.IsAbs(h.Path) {
			continue
		}
		deny := false
		for _, p := range pathForms(h.Path) {
			for _, b := range blocked {
				if within(p, b) || within(b, p) {
					deny = true
				}
			}
		}
		if !deny {
			h.Path = filepath.Clean(h.Path)
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// pathForms gives a cleaned path and its resolved form when available.
func pathForms(p string) []string {
	out := []string{filepath.Clean(p)}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		out = append(out, filepath.Clean(real))
	}
	return out
}

// within says path equals dir or lies below it, never just shares its prefix.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
