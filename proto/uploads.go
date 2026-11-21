package proto

import (
	"path/filepath"
	"strings"
)

// The rules of an upload's name and type (rule 10a): pocketd keeps them,
// and so does its fake in the desktop's tests, so those tests read the
// lines pocketd writes (Codex review of #49, round 8).

// maxName is the most bytes of a safe name: MaxAttachedName bounds what a
// message's line may name.
const maxName = 120

// SafeUploadName is name as pi-pocket keeps it, and pocketd with it: its
// last path element, each run of characters other than ASCII letters,
// digits, "_", ".", "-", space, "(" and ")" made one "_", trimmed of
// spaces, with "upload" before it when it is empty or starts with ".", and
// at most 120 bytes. It holds no comma, so a message's line names it
// unmistakably (AttachmentLine), and it is its own safe name: "upload"
// comes before the cut, so a second pass changes nothing (Codex review of
// #49, round 1).
func SafeUploadName(name string) string {
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
	if safe == "" || strings.HasPrefix(safe, ".") {
		safe = "upload" + safe
	}
	if len(safe) > maxName {
		safe = strings.TrimSpace(safe[:maxName])
	}
	return safe
}

// uploadTypes are the types of the extensions pocketd knows, after
// pi-pocket's table: pocketd never asks the host, so every host gives one
// name one type.
var uploadTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".heic": "image/heic", ".heif": "image/heif", ".svg": "image/svg+xml", ".bmp": "image/bmp",
	".pdf": "application/pdf", ".json": "application/json", ".zip": "application/zip",
	".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
	".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript",
	".xml": "application/xml", ".yaml": "application/yaml", ".yml": "application/yaml",
}

// UploadMime is an upload's type, as pocketd keeps it: the declared one (a
// Content-Type) without its parameters, in lowercase, when it is a type of
// at most MaxMimeType bytes other than application/octet-stream; else the
// type of name's extension; else application/octet-stream.
func UploadMime(name, declared string) string {
	declared, _, _ = strings.Cut(declared, ";")
	declared = strings.ToLower(strings.TrimSpace(declared))
	if declared != "application/octet-stream" && len(declared) <= MaxMimeType && mimeType.MatchString(declared) {
		return declared
	}
	if t, ok := uploadTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
