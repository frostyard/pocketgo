package team

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A session's instructions hold what pocketd reads for it when it starts
// (ADR-0043): the owner's global AGENTS.md, its folder's AGENTS.md and the
// skills it may read. pocketd reads them as the agents' sandbox shows
// them; the sections below are what it writes of them, and a file it
// leaves out is a note, never a session that fails.

// MaxInstructionsFile bounds the global AGENTS.md and a folder's
// AGENTS.md, each: a larger one is left out with a note.
const MaxInstructionsFile = 64 << 10

// MaxSkillsListed bounds the skills listed from one skills folder: the
// first, by name, and a note for the rest.
const MaxSkillsListed = 100

// FileRead is one file as pocketd read it for a session's instructions:
// Found says it is a regular file the sandbox shows, Data is at most its
// bound and one byte more, Names its hard links, and Err a read that
// failed. Path is where pocketd looked, which the agent's read tool
// reads too.
type FileRead struct {
	Path  string
	Found bool
	Data  []byte
	Names uint64
	Err   string
}

// SkillRead is one folder of a skills folder: its name and its SKILL.md,
// at most profiles.MaxSkillHead and one byte more.
type SkillRead struct {
	Name string
	File FileRead
}

// SkillsRead is a skills folder as pocketd read it: its folders with a
// SKILL.md, by name, at most MaxSkillsListed; More says more are there.
// ReadOnly is the global skills folder, which agents cannot change
// (ADR-0035): a SKILL.md there with another name could change outside it,
// so it is left out, as a gather leaves it out (ADR-0033).
type SkillsRead struct {
	Dir      string
	Found    bool
	Skills   []SkillRead
	More     bool
	ReadOnly bool
	Err      string
}

// Loaded is what pocketd wrote into a session's instructions of what it
// read when the session started, or started over: each part's section,
// "" for none.
type Loaded struct {
	Global, Folder, Skills string
}

// GlobalSection is the global AGENTS.md's section, the owner's rules for
// every session. A global file with more than one name is left out with a
// note, as a global skill is (ADR-0045).
func GlobalSection(r FileRead) string {
	if r.Err == "" && r.Found && r.Names > 1 {
		return LeftOutNote(r.Path, "it has more than one name (hard links), through which it could change outside its read-only bind")
	}
	text, note := fileText(r)
	switch {
	case note != "":
		return note
	case text == "":
		return ""
	}
	return "The owner's rules for every session follow, from " + r.Path + ".\n\n" + text + "\n\nEnd of the owner's rules."
}

// FolderSection is the section of the AGENTS.md of the session's folder.
func FolderSection(r FileRead) string {
	text, note := fileText(r)
	switch {
	case note != "":
		return note
	case text == "":
		return ""
	}
	return "The instructions of your folder follow, from " + r.Path + ".\n\n" + text + "\n\nEnd of " + r.Path + "."
}

// fileText is r's text, trimmed, or the note that says why pocketd left
// it out: "" for both when there is no file, or nothing in it.
func fileText(r FileRead) (text, note string) {
	switch {
	case r.Err != "":
		return "", LeftOutNote(r.Path, "pocketd could not read it ("+r.Err+")")
	case !r.Found:
		return "", ""
	case len(r.Data) > MaxInstructionsFile:
		return "", LeftOutNote(r.Path, fmt.Sprintf("it is larger than %d bytes", MaxInstructionsFile))
	case !utf8.Valid(r.Data):
		return "", LeftOutNote(r.Path, "it is not UTF-8")
	}
	text = strings.TrimSpace(strings.ReplaceAll(string(r.Data), "\r\n", "\n"))
	if line, ok := lessonsLine(text); ok {
		return "", LeftOutNote(r.Path, fmt.Sprintf("it holds the line %q, which marks the lessons pocketd writes", line))
	}
	return text, ""
}

// lessonsLine is a line of text that marks the lessons section, which
// pocketd owns from its marker on (ADR-0015, WithoutLessons): a text that
// held one could lose its rest, or take a section's place, at a refresh.
func lessonsLine(text string) (string, bool) {
	for line := range strings.SplitSeq(text, "\n") {
		if line == profiles.LessonsMarker || line == profiles.LessonsEnd {
			return line, true
		}
	}
	return "", false
}

// LeftOutNote tells the agent pocketd left path out of its instructions,
// and why: it may read it itself.
func LeftOutNote(path, why string) string {
	return "pocketd left " + path + " out of these instructions: " + why + ". Read it with your read tool if you need it."
}

// UnreadNote tells the agent pocketd could not read the files and the
// skills folders its instructions would hold, and why: the session starts
// without them, and the agent may read them itself.
func UnreadNote(why string, files, skills []string) string {
	text := "pocketd could not read the files these instructions hold (" + why + "), so they are left out:"
	for _, path := range files {
		text += "\n- " + path
	}
	for _, dir := range skills {
		text += "\n- the skills in " + dir + " (<name>/" + profiles.SkillFile + ")"
	}
	return text + "\nRead them with your read tool if you need them."
}

// SkillsSection lists the skills of folders, in their order, by name,
// description and file, with a note for each skill left out and for a
// folder listed in part: "" when there is neither.
func SkillsSection(folders []SkillsRead) string {
	var listed, notes []string
	for _, folder := range folders {
		if folder.Err != "" {
			notes = append(notes, "pocketd could not list the skills in "+folder.Dir+" ("+folder.Err+").")
			continue
		}
		for _, skill := range folder.Skills {
			// A folder's name is written quoted until it is checked: it
			// could hold a line break.
			if err := profiles.CheckSkillName(skill.Name); err != nil {
				notes = append(notes, "pocketd left out a folder in "+folder.Dir+": "+err.Error()+".")
				continue
			}
			description, why := skillLine(folder, skill)
			if why != "" {
				notes = append(notes, "pocketd left out the skill "+skill.File.Path+": "+why+".")
				continue
			}
			listed = append(listed, "- "+skill.Name+": "+description+" ("+skill.File.Path+")")
		}
		if folder.More {
			notes = append(notes, fmt.Sprintf("pocketd listed the first %d skills in %s, by name: more are there.", MaxSkillsListed, folder.Dir))
		}
	}
	if len(listed) == 0 && len(notes) == 0 {
		return ""
	}
	text := "Skills: when a task matches a skill's description, read its file with your read tool and follow it."
	if len(listed) > 0 {
		text += "\n\n" + strings.Join(listed, "\n")
	} else {
		text += "\n\nNo skill is listed."
	}
	if len(notes) > 0 {
		text += "\n\n" + strings.Join(notes, "\n")
	}
	return text
}

// skillLine is skill's description, or why it is left out.
func skillLine(folder SkillsRead, skill SkillRead) (description, why string) {
	switch {
	case skill.File.Err != "":
		return "", "pocketd could not read it (" + skill.File.Err + ")"
	case folder.ReadOnly && skill.File.Names > 1:
		return "", "it has more than one name (hard links), through which it could change outside the skills folder"
	}
	description, err := profiles.ParseSkillHead(skill.Name, skill.File.Data)
	if err != nil {
		return "", err.Error()
	}
	return description, ""
}

// Instructions joins a session's instructions from its parts, in their
// order, each after a blank line, the empty ones left out; the lessons
// section, which begins with its own blank line, ends them (ADR-0043).
func Instructions(lessons string, parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n") + lessons
}
