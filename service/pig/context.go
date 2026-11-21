package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MichaelKinsy/PiG/durable"

	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// contextAnswerRoom bounds ContextHelper's answer: two files and two
// skills folders at their bounds, in JSON's base64, with room to spare.
const contextAnswerRoom = 4 << 20

// loadContext is what session spec's instructions hold of the files
// pocketd reads for it (ADR-0043): the global AGENTS.md
// (Options.Instructions), the AGENTS.md of its folder, and the skills of
// the global skills folder (Options.Skills) and of its folder's
// .pi/skills. A cross-review reads nothing of its folder, the change it
// reviews. All is read in one ContextHelper run in the sandbox, as the
// agents' own tools would read it; a file that is not there, too large or
// wrong is no part or a note, and a read the sandbox could not make is a
// note too (team.UnreadNote): the session starts all the same.
func (e *Engine) loadContext(ctx context.Context, spec api.SessionSpec) team.Loaded {
	var request contextRequest
	global, folder := -1, -1
	if e.opts.Instructions != "" {
		global = len(request.Files)
		request.Files = append(request.Files, fileRequest{Path: e.opts.Instructions, Max: team.MaxInstructionsFile})
	}
	cwd := ""
	if !spec.Review && filepath.IsAbs(spec.Cwd) {
		cwd = filepath.Clean(spec.Cwd)
	}
	if cwd != "" {
		folder = len(request.Files)
		request.Files = append(request.Files, fileRequest{Path: filepath.Join(cwd, "AGENTS.md"), Max: team.MaxInstructionsFile})
	}
	var dirs []string
	readOnly := map[string]bool{}
	if e.opts.Skills != "" {
		dir := filepath.Clean(e.opts.Skills)
		dirs, readOnly[dir] = append(dirs, dir), true
	}
	if cwd != "" {
		// The folder's own skills, unless they are the global ones.
		if dir := filepath.Join(cwd, ".pi", "skills"); !readOnly[dir] {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs {
		request.Skills = append(request.Skills, skillsRequest{Dir: dir, Max: team.MaxSkillsListed, Head: profiles.MaxSkillHead})
	}
	if len(request.Files) == 0 && len(request.Skills) == 0 {
		return team.Loaded{}
	}
	answer, err := e.readContext(ctx, request)
	if err != nil {
		var paths []string
		for _, f := range request.Files {
			paths = append(paths, f.Path)
		}
		return team.Loaded{Global: team.UnreadNote(err.Error(), paths, dirs)}
	}
	var loaded team.Loaded
	if global >= 0 {
		loaded.Global = team.GlobalSection(fileRead(request.Files[global].Path, answer.Files[global]))
	}
	if folder >= 0 {
		loaded.Folder = team.FolderSection(fileRead(request.Files[folder].Path, answer.Files[folder]))
	}
	var folders []team.SkillsRead
	for i, dir := range dirs {
		got := answer.Skills[i]
		read := team.SkillsRead{Dir: dir, Found: got.Found, More: got.More, ReadOnly: readOnly[dir], Err: got.Err}
		for _, skill := range got.Skills {
			read.Skills = append(read.Skills, team.SkillRead{Name: skill.Name, File: fileRead(filepath.Join(dir, skill.Name, profiles.SkillFile), skill.File)})
		}
		folders = append(folders, read)
	}
	loaded.Skills = team.SkillsSection(folders)
	return loaded
}

// fileRead is the helper's answer for path as team reads it.
func fileRead(path string, a fileAnswer) team.FileRead {
	return team.FileRead{Path: path, Found: a.Found, Data: a.Data, Names: a.Names, Err: a.Err}
}

// readContext runs ContextHelper for request in the sandbox, and checks
// that its answer has an entry for each of the request's.
func (e *Engine) readContext(ctx context.Context, request contextRequest) (contextAnswer, error) {
	self := e.opts.Self
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return contextAnswer{}, err
		}
	}
	arg, err := json.Marshal(request)
	if err != nil {
		return contextAnswer{}, err
	}
	cmd, err := e.sandboxed(ctx, []string{self, ContextHelper, string(arg)})
	if err != nil {
		return contextAnswer{}, err
	}
	stdout := &capped{max: contextAnswerRoom}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return contextAnswer{}, fmt.Errorf("read the session's instructions files: %w", err)
	}
	if stdout.dropped > 0 {
		return contextAnswer{}, errors.New("read the session's instructions files: the answer is too long")
	}
	var answer contextAnswer
	if err := json.Unmarshal(stdout.buf, &answer); err != nil {
		return contextAnswer{}, fmt.Errorf("read the session's instructions files: %w", err)
	}
	if len(answer.Files) != len(request.Files) || len(answer.Skills) != len(request.Skills) {
		return contextAnswer{}, errors.New("read the session's instructions files: the answer does not match the request")
	}
	for _, s := range answer.Skills {
		if len(s.Skills) > team.MaxSkillsListed {
			return contextAnswer{}, errors.New("read the session's instructions files: too many skills in the answer")
		}
	}
	return answer, nil
}

// loaded is what a session's instructions hold of what pocketd read for
// it when it started, or last started over (team.Loaded), kept by session
// id in the commit that writes them: a standing session's refresh writes
// its instructions again with them, and reads no file (ADR-0043). A
// session made before has none, which is no part.
type loaded struct {
	Global string `json:"global,omitempty"`
	Folder string `json:"folder,omitempty"`
	Skills string `json:"skills,omitempty"`
}

var loadedDoc = durable.DefineDocFamily(durable.DocFamilyDefinition[loaded, loaded]{
	Family: true, Kind: "pocketgo.loaded", Version: 1,
	DocumentSemantics: durable.DocumentSemantics{Scope: durable.ScopeSession},
	Initial:           func(seed loaded) loaded { return seed },
	CheckpointWhen:    func(loaded, []durable.Op, durable.CheckpointInfo) bool { return false },
})

// keepLoaded keeps l as session id's loaded parts in tx.
func keepLoaded(tx durable.Tx, id int64, l team.Loaded) error {
	d, err := durable.TxDoc[loaded](tx, loadedDoc, pickKey(id), loaded{})
	if err != nil {
		return err
	}
	return setFields(d, map[string]any{"global": l.Global, "folder": l.Folder, "skills": l.Skills})
}

// loadedOf is session id's loaded parts as keepLoaded kept them, none for
// a session made before.
func (e *Engine) loadedOf(ctx context.Context, id int64) (team.Loaded, error) {
	found, err := durable.Snapshot[loaded](ctx, e.harness, loadedDoc, pickKey(id))
	if err != nil || found == nil {
		return team.Loaded{}, err
	}
	return team.Loaded{Global: found.Global, Folder: found.Folder, Skills: found.Skills}, nil
}
