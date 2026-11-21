package proto

import (
	"regexp"
	"strconv"
	"strings"
)

// AttachmentsHeading is the line before the attached files' lines at the
// end of a message that carries attachments (ADR-0029), as pi-pocket
// writes it: pocketd writes it, and the desktop splits the message there.
const AttachmentsHeading = "Attached files (saved on the server):"

// AttachedFile is a file a message names, as its line says it: where it is
// on the service host, its name, its type, its size in bytes, and a note
// on what the model got of it, "" for none.
type AttachedFile struct {
	Path, Name, MimeType string
	Size                 int64
	Note                 string
}

// AttachmentLine is f's line in a message:
// "- <path> (<name>, <type>, <size> bytes)", then " <note>" when it has one.
func AttachmentLine(f AttachedFile) string {
	line := "- " + f.Path + " (" + f.Name + ", " + f.MimeType + ", " + strconv.FormatInt(f.Size, 10) + " bytes)"
	if f.Note != "" {
		line += " " + f.Note
	}
	return line
}

// WithAttachments is the message of text that carries files: text, a blank
// line, AttachmentsHeading and a line for each file. A text of white space
// alone gives the heading and the lines. No files give text as it is.
func WithAttachments(text string, files []AttachedFile) string {
	if len(files) == 0 {
		return text
	}
	lines := make([]string, len(files))
	for i, f := range files {
		lines[i] = AttachmentLine(f)
	}
	list := AttachmentsHeading + "\n" + strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return list
	}
	return text + "\n\n" + list
}

// attachmentTail is what follows a file's path on its line: its name, its
// type, its size and its note. A safe name holds no comma (uploads.SafeName).
var attachmentTail = regexp.MustCompile(`^([^,\n]+), ([a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*), ([0-9]+) bytes\)(?: (.+))?$`)

// SplitAttachments reads the files a message names, as WithAttachments
// wrote them: words is the message without them. A message whose last
// AttachmentsHeading is not followed by file lines alone, each a path
// ending in "-<name>" as pocketd stores a file, names none: words is the
// whole message.
func SplitAttachments(text string) (words string, files []AttachedFile) {
	var list string
	if i := strings.LastIndex(text, "\n\n"+AttachmentsHeading+"\n"); i >= 0 {
		words, list = text[:i], text[i+len("\n\n"+AttachmentsHeading+"\n"):]
	} else if rest, ok := strings.CutPrefix(text, AttachmentsHeading+"\n"); ok {
		list = rest
	} else {
		return text, nil
	}
	for line := range strings.SplitSeq(list, "\n") {
		f, ok := parseAttachmentLine(line)
		if !ok {
			return text, nil
		}
		files = append(files, f)
	}
	return words, files
}

// parseAttachmentLine reads one file's line. A path may hold " (", as a
// name may: the split is the first one after which the rest reads as a
// line's tail and the path ends in "-<name>".
func parseAttachmentLine(line string) (AttachedFile, bool) {
	rest, ok := strings.CutPrefix(line, "- /")
	if !ok {
		return AttachedFile{}, false
	}
	rest = "/" + rest
	for i := 0; ; {
		j := strings.Index(rest[i:], " (")
		if j < 0 {
			return AttachedFile{}, false
		}
		at := i + j
		path, tail := rest[:at], rest[at+2:]
		if m := attachmentTail.FindStringSubmatch(tail); m != nil && strings.HasSuffix(path, "-"+m[1]) {
			size, err := strconv.ParseInt(m[3], 10, 64)
			if err == nil {
				return AttachedFile{Path: path, Name: m[1], MimeType: m[2], Size: size, Note: m[4]}, true
			}
		}
		i = at + 2
	}
}
