package proto

import (
	"reflect"
	"testing"
)

// A message with attachments ends with the heading and one line a file, as
// pi-pocket writes it, and reads back as its words and its files: a path
// and a name may hold spaces and parentheses (ADR-0029).
func TestAMessagesAttachmentsReadBackAsWritten(t *testing.T) {
	files := []AttachedFile{
		{Path: "/home/team/.local/share/pocketgo-uploads/7/m3x9k2-ab12cd34-shot.png", Name: "shot.png", MimeType: "image/png", Size: 12345,
			Note: "[Image: original 4000x3000, displayed at 2000x1500. Multiply coordinates by 2.00 to map to original image.]"},
		{Path: "/home/my team (x)/.local/share/pocketgo-uploads/7/m3x9k3-0011aabb-report (1).pdf", Name: "report (1).pdf", MimeType: "application/pdf", Size: 99},
	}
	text := WithAttachments("Look at these.", files)
	want := "Look at these.\n\n" + AttachmentsHeading + "\n" +
		"- /home/team/.local/share/pocketgo-uploads/7/m3x9k2-ab12cd34-shot.png (shot.png, image/png, 12345 bytes) [Image: original 4000x3000, displayed at 2000x1500. Multiply coordinates by 2.00 to map to original image.]\n" +
		"- /home/my team (x)/.local/share/pocketgo-uploads/7/m3x9k3-0011aabb-report (1).pdf (report (1).pdf, application/pdf, 99 bytes)"
	if text != want {
		t.Fatalf("message\n%q\nwant\n%q", text, want)
	}
	words, got := SplitAttachments(text)
	if words != "Look at these." || !reflect.DeepEqual(got, files) {
		t.Fatalf("read back %q, %+v", words, got)
	}

	// Files alone: the heading starts the message.
	alone := WithAttachments("  \n", files[:1])
	if alone != AttachmentsHeading+"\n"+AttachmentLine(files[0]) {
		t.Fatalf("files alone: %q", alone)
	}
	if words, got := SplitAttachments(alone); words != "" || !reflect.DeepEqual(got, files[:1]) {
		t.Fatalf("files alone read back %q, %+v", words, got)
	}
	// No files: the text as it is.
	if WithAttachments("hi", nil) != "hi" {
		t.Fatal("a message without files changed")
	}
}

// Only pocketd's own lines are files: a message that quotes the heading,
// or a list with a line that is no file's, names none, and the last
// heading is the one pocketd wrote.
func TestOnlyFileLinesAreAttachments(t *testing.T) {
	line := AttachmentLine(AttachedFile{Path: "/u/7/a-b-x.txt", Name: "x.txt", MimeType: "text/plain", Size: 1})
	for _, text := range []string{
		"plain words",
		"quoted:\n\n" + AttachmentsHeading + "\n- not a file",
		"quoted:\n\n" + AttachmentsHeading + "\n" + line + "\nmore words",
		"quoted:\n\n" + AttachmentsHeading + "\n- relative/a-b-x.txt (x.txt, text/plain, 1 bytes)",
		"quoted:\n\n" + AttachmentsHeading + "\n- /u/7/other.txt (x.txt, text/plain, 1 bytes)",
		"quoted:\n\n" + AttachmentsHeading + "\n- /u/7/a-b-x.txt (x.txt, Text/Plain, 1 bytes)",
	} {
		if words, files := SplitAttachments(text); words != text || files != nil {
			t.Errorf("%q split into %q, %+v", text, words, files)
		}
	}
	quoted := "I wrote:\n\n" + AttachmentsHeading + "\n- earlier\n\nnow:"
	text := WithAttachments(quoted, []AttachedFile{{Path: "/u/7/a-b-x.txt", Name: "x.txt", MimeType: "text/plain", Size: 1}})
	if words, files := SplitAttachments(text); words != quoted || len(files) != 1 || files[0].Name != "x.txt" {
		t.Fatalf("split %q, %+v", words, files)
	}
}
