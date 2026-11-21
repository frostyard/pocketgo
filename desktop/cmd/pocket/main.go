// Command pocket is the pocketgo desktop client: a native window on MyGo
// and frostui over pocketd's protocol (docs/specs/protocol.md). Its app ID
// is org.frostyard.PocketGo, so it runs beside frostyard/pocket.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/app"
	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/icons"
)

// version is stamped at build time.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pocket:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := frostui.SetAppID(config.AppID, "PocketGo"); err != nil {
		slog.Warn("setting the app ID", "err", err)
	}
	// On Linux the name is the desktop's app ID, which matches the window
	// to its desktop entry. On macOS and Windows a packaged app takes the
	// name it was built with ("PocketGo"), which its menus show.
	if runtime.GOOS == "linux" {
		mygo.App.SetName(config.AppID)
	}
	frostui.SetIcons(frostui.NewIcons(icons.Embedded()))

	store, err := config.Open(os.Getenv("POCKETGO_CONFIG_DIR"))
	if err != nil {
		return err
	}
	p := &poster{}
	var win *mygo.Window
	auth := newGitHubAuth()
	a, err := app.New(app.Options{
		Version: version,
		Post:    p.post,
		Store:   store,
		Creds:   creds(store.Dir()),
		Quit:    mygo.App.Quit,
		// The owner's GitHub App sign-in, for approve and merge (ADR-0018).
		GitHubAuth:   auth,
		GitHubClient: githubClient,
		// The native chooser of Attach files (ADR-0029).
		PickFiles: pickFiles,
	})
	if err != nil {
		return err
	}
	mygo.App.WhenReady(func() {
		win = mygo.NewWindow(mygo.WindowOptions{
			Title: "PocketGo", Width: 1280, Height: 820, MinWidth: 760, MinHeight: 480,
			TitleBarStyle: mygo.TitleBarHidden,
			Content:       ui.View(a.View),
		})
		win.OnClose(func(*mygo.CloseEvent) {
			a.Close()
			auth.Close()
		})
		p.attach(win)
		a.Start()
	})
	return mygo.App.Run()
}

// poster hands functions to the window's UI thread. It is safe from any
// goroutine and keeps what comes before the window exists until attach.
type poster struct {
	mu      sync.Mutex
	win     *mygo.Window
	pending []func()
}

func (p *poster) post(fn func()) {
	p.mu.Lock()
	if p.win == nil {
		p.pending = append(p.pending, fn)
		p.mu.Unlock()
		return
	}
	win := p.win
	p.mu.Unlock()
	win.Update(fn)
}

func (p *poster) attach(win *mygo.Window) {
	p.mu.Lock()
	p.win = win
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()
	for _, fn := range pending {
		win.Update(fn)
	}
}

// creds keeps device tokens in the desktop's keyring. Only where there is
// none (errors.ErrUnsupported) does config.Credentials use a file of the
// config folder; nothing else switches the keyring off.
func creds(dir string) config.Credentials {
	return config.Credentials{Ring: secrets{}, NotFound: mygo.ErrSecretNotFound, Dir: dir}
}

// secrets is mygo.Secrets as a config.Keyring.
type secrets struct{}

// keyring is the credential store's service: the app's identifier
// whether it runs packaged or with go run, so both find one sign-in.
var keyring = mygo.Secrets.Service(config.AppID)

func (secrets) Set(key string, value []byte) error { return keyring.Set(key, value) }
func (secrets) Get(key string) ([]byte, error)     { return keyring.Get(key) }
func (secrets) Delete(key string) error {
	err := keyring.Delete(key)
	if errors.Is(err, mygo.ErrSecretNotFound) {
		return nil
	}
	return err
}
