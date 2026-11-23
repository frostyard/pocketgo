package files

import (
	"path"
	"strings"
)

// IsImageName says an extension, in any case, is .png, .jpg, .jpeg, .gif or .webp.
func IsImageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// IsMarkdownName says a text file's extension is .md or .markdown, in any case.
func IsMarkdownName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return true
	}
	return false
}
