// Command preview serves the packaged web app and an isolated in-memory pocketd
// fixture on one loopback origin. The desktop app can connect to the same fixture.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func main() {
	web := flag.String("web", "cmd/pocket-nsgo/dist/PocketGo-0.1.0-web", "built web directory")
	port := flag.Int("port", 0, "loopback port (0 chooses an available port)")
	scene := flag.String("scene", "busy-team", "preview scene: busy-team or checklist")
	flag.Parse()
	if _, err := os.Stat(filepath.Join(*web, "index.html")); err != nil {
		log.Fatal(err)
	}
	s := pocketdtest.New()
	model := proto.ModelRef{Provider: "fixture", ID: "reviewer"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Reviewer"})
	s.AddProfile(proto.Profile{ID: "maintainer", Name: "Maintainer", Cwd: "/workspace", Runs: model})
	s.AddSession(proto.Session{ID: 3, Title: "Release checklist", Profile: "maintainer", Cwd: "/workspace", Model: model, State: proto.SessionIdle}, proto.Entry{ID: 1, Kind: proto.EntryUser, Text: "What should we check before tagging the release?"}, proto.Entry{ID: 2, Kind: proto.EntryAssistant, Thinking: "Reading the release notes.", Text: "The release checklist is ready.\n\n- Verify the desktop build.\n- Exercise the browser client.\n- Record the results in the release notes."})
	s.SetPrompts(proto.Prompts{Prompts: []proto.Prompt{{Name: "review", Text: "Review the release notes for $ARGUMENTS."}}})
	s.AddThread(proto.Thread{ID: 1, Title: "Prepare release notes", Session: 3, Agent: "maintainer", State: proto.ThreadOpen})
	s.SetFileRoots(proto.FileRoots{Roots: []proto.FileRoot{{ID: "maintainer", Name: "Maintainer", Path: "/workspace"}}})
	s.SetFileFolder(proto.FileFolder{Root: "maintainer", Entries: []proto.FileEntry{{Name: "release.md", Kind: "file"}}})
	s.SetFileContent(proto.FileContent{Root: "maintainer", Path: "release.md", Kind: "text", Text: "# Release notes\n\nThe build is ready for review."})
	switch *scene {
	case "busy-team":
		pocketdtest.AddBusyTeam(s)
	case "checklist":
	default:
		log.Fatalf("unknown preview scene %q", *scene)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", s.Handler())
	mux.Handle("/", http.FileServer(http.Dir(*web)))
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Preview and service URL  http://%s\nFixture device token     %s\n", l.Addr(), pocketdtest.Token)
	server := http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.Serve(l))
}
