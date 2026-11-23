package nsgoclient

import (
	"reflect"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/desktop/workspaceui"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/ui/chat"
)

func (c *client) modelControls(cx *core.Scope, id int64) core.Node {
	menu := core.NewMemoFunc(cx, func() state.ModelChip {
		c.revision.Get()
		s, _ := c.store.Data.Session(id)
		models, why := c.store.Models()
		return state.ChipOf(s, models, why)
	}, func(a, b state.ModelChip) bool { return reflect.DeepEqual(a, b) })
	return core.Dynamic(func(*core.Scope) core.Node {
		chip := menu.Get()
		if chip.Reload {
			return ui.Button{Text: "Reload models", Label: "Reload models", Variant: ui.ButtonGhost, OnPress: func() { c.store.LoadModels(); c.changed() }}
		}
		var models, levels []chat.Option
		refs := map[string]proto.ModelRef{}
		var selected, effort string
		label := chip.Label
		for _, g := range chip.Providers {
			for _, m := range g.Models {
				models = append(models, chat.Option{ID: m.ID, Label: m.Label, Group: g.Provider})
				refs[m.ID] = m.Ref
				if m.Current {
					selected, label = m.ID, m.Label
				}
			}
		}
		if label == "" {
			label = "Model"
		}
		if len(models) == 0 {
			return core.Text{Content: label, Style: ui.TypeCaption}
		}
		nodes := []core.Node{chat.Picker{Label: func() string { return label }, Options: models, Selected: func() string { return selected }, SearchLabel: "Search models", EmptyText: "No matching models", TestID: "composer-model", OnChange: func(key string) {
			if ref, ok := refs[key]; ok {
				c.store.ChangeModel(id, &ref, "")
				c.changed()
			}
		}}}
		for _, l := range chip.Levels {
			levels = append(levels, chat.Option{ID: l.Level, Label: l.Level})
			if l.Current {
				effort = l.Level
			}
		}
		if len(levels) > 1 {
			nodes = append(nodes, chat.EffortPicker{Label: "Thinking level", TestID: "composer-effort", Levels: levels, Off: levels[0].ID == "off", Selected: func() string { return effort }, OnChange: func(level string) { c.store.ChangeModel(id, nil, level); c.changed() }})
		}
		return workspaceui.Row(nodes...)
	})
}
