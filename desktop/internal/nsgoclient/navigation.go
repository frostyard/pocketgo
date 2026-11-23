package nsgoclient

import (
	"fmt"
	"strings"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
)

type destination struct {
	page           string
	session, entry int64
	card           string
	file           state.FileSpot
	lookup         bool
	details        bool
}

type visit struct {
	dest    destination
	address string
}

type navigation struct {
	visits        []visit
	index, serial int
	revision      *core.Signal[int]
}

func (c *client) resetHistory() {
	c.nav.visits = nil
	c.nav.index = 0
	c.record(destination{page: "Inbox"})
}

func (c *client) record(dest destination) {
	n := &c.nav
	if len(n.visits) > 0 && n.visits[n.index].dest == dest {
		return
	}
	mode := core.HistoryReplace
	if len(n.visits) > 0 {
		n.visits = n.visits[:n.index+1]
		mode = core.HistoryPush
	}
	n.serial++
	v := visit{dest: dest, address: fmt.Sprintf("#pocket-%d", n.serial)}
	n.visits = append(n.visits, v)
	if len(n.visits) > 100 {
		n.visits = n.visits[1:]
	}
	n.index = len(n.visits) - 1
	if c.browserHistory {
		_ = c.cx.SetAppURL(v.address, core.AppURLOptions{History: mode})
	}
	n.revision.Update(func(v int) int { return v + 1 })
}

// Remember the source card even when reached by scrolling rather than a link.
func (c *client) rememberCard(key string) { c.nav.visits[c.nav.index].dest.card = key }

func (c *client) goTo(dest destination) {
	c.record(dest)
	c.showDestination(dest)
}

func (c *client) canMove(delta int) bool {
	c.nav.revision.Get()
	i := c.nav.index + delta
	return i >= 0 && i < len(c.nav.visits)
}
func (c *client) move(delta int) bool {
	if !c.canMove(delta) {
		return false
	}
	if c.browserHistory && moveBrowser(delta) {
		return true
	}
	c.restoreVisit(c.nav.index + delta)
	return true
}
func (c *client) onAddress(change core.AppURLChange) {
	_, fragment, ok := strings.Cut(change.URL, "#")
	if !ok || !c.store.SignedIn() {
		return
	}
	for i, v := range c.nav.visits {
		if v.address == "#"+fragment {
			c.restoreVisit(i)
			return
		}
	}
}
func (c *client) restoreVisit(i int) {
	c.nav.index = i
	c.nav.revision.Update(func(v int) int { return v + 1 })
	c.showDestination(c.nav.visits[i].dest)
}

func (c *client) showDestination(d destination) {
	c.cx.Batch(func() {
		c.noticeCard.Set(d.card)
		c.seek.Set(d.entry)
		c.page.Set(d.page)
		switch d.page {
		case "Session":
			c.store.Open(d.session)
			c.prefs.LastSession = d.session
			if s, ok := c.store.Data.Session(d.session); ok {
				c.prefs.Seen, _ = c.prefs.Seen.Saw(s)
			}
			c.savePrefs()
			c.session.Set(d.session)
		case "Files":
			c.fileRoute.Set(d)
			if c.store.Data.FileRoots().Status != state.ListLoaded {
				c.store.LoadFileRoots()
			}
		case "Terminal":
			c.store.LoadTerminals()
		case "New session":
			c.store.LoadModels()
			c.store.LoadProfiles()
		}
		c.changed()
	})
}
