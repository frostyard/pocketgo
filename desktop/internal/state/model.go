package state

import (
	"context"
	"fmt"

	"github.com/frostyard/pocketgo/proto"
)

// The composer's model chip, frostyard/pocket's (ADR-0010, ADR-0028): it
// shows the session's model and its thinking level, and a menu changes
// them, with one submenu per provider listing its models, the current one
// checked, and a Thinking submenu when the model takes more than one level.

// ModelChip is what the chip of a session shows and offers.
type ModelChip struct {
	// Label is the model's name, its id when the models do not name it,
	// then its thinking level when it has one other than off.
	Label string
	// Providers are the models pocketd runs, by provider, in the order
	// GET /v1/models lists them.
	Providers []ModelGroup
	// Levels are the session's model's thinking levels, lowest first;
	// none when it takes one level or fewer.
	Levels []LevelItem
}

// ModelGroup is a provider's models.
type ModelGroup struct {
	Provider string
	Models   []ModelItem
}

// ModelItem is a model the chip offers.
type ModelItem struct {
	Ref proto.ModelRef
	// Label is its name, its id when it has none.
	Label string
	// Current: the session runs it.
	Current bool
}

// LevelItem is a thinking level the chip offers.
type LevelItem struct {
	Level string
	// Current: the session thinks at it. An empty level, the model's
	// default, reads as off, as PiG reads it.
	Current bool
}

// ChipOf is the model chip of session s, with the models pocketd runs.
func ChipOf(s proto.Session, models []proto.Model) ModelChip {
	chip := ModelChip{Label: s.Model.ID}
	level := s.Thinking
	if level == "" {
		level = "off"
	}
	at := map[string]int{}
	for _, m := range models {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		current := m.ModelRef == s.Model
		if current {
			chip.Label = label
			if len(m.ThinkingLevels) > 1 {
				for _, l := range m.ThinkingLevels {
					chip.Levels = append(chip.Levels, LevelItem{Level: l, Current: l == level})
				}
			}
		}
		i, ok := at[m.Provider]
		if !ok {
			i = len(chip.Providers)
			at[m.Provider] = i
			chip.Providers = append(chip.Providers, ModelGroup{Provider: m.Provider})
		}
		chip.Providers[i].Models = append(chip.Providers[i].Models, ModelItem{Ref: m.ModelRef, Label: label, Current: current})
	}
	if level != "off" {
		chip.Label += " · " + level
	}
	return chip
}

// ModelChanged applies the answer of POST /v1/sessions/{id}/model, as the
// session's event would: an event that changed the session since f began
// wins over the answer's copy.
func (d *Data) ModelChanged(f Fetch, s proto.Session) { d.Created(f, s) }

// ChangeModel changes session id's model, its thinking level, or both
// (POST /v1/sessions/{id}/model): a nil model keeps the session's, and an
// empty level keeps its level when the model takes it, else the model's
// default. ChangingModel is where it stands. A change tried again
// unchanged keeps its request id; another change is a new request.
func (s *Store) ChangeModel(id int64, model *proto.ModelRef, thinking string) {
	p := pending(s.modelChanges, id)
	if p.Busy {
		return
	}
	target := "-"
	if model != nil {
		target = model.String()
	}
	key := fmt.Sprintf("model:%d", id)
	request := proto.ChangeModel{RequestID: s.ids.For(key, target+" "+thinking), Model: model, Thinking: thinking}
	p.Busy, p.Err = true, ""
	if model != nil {
		s.log(LevelCommand, "Model %s for session %d", target, id)
	} else {
		s.log(LevelCommand, "Thinking %s for session %d", thinking, id)
	}
	var session proto.Session
	f := s.Data.Begin()
	s.do(fmt.Sprintf("Changing the model of session %d", id), func(ctx context.Context, c *proto.Client) (err error) {
		session, err = c.ChangeModel(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		if err != nil {
			p.Err = err.Error()
			return
		}
		s.ids.Done(key)
		s.Data.ModelChanged(f, session)
	})
}

// ChangingModel is where the owner's change of session id's model stands.
func (s *Store) ChangingModel(id int64) Pending { return valueOf(s.modelChanges[id]) }
