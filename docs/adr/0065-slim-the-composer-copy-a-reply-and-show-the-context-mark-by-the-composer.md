# 0065 — Slim the composer, copy a reply, and show the context mark by the composer

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

**The composer took more room than the message.** Under its field it had three full buttons: Attach files, the model chip with its frame ([ADR-0028](0028-let-the-owner-change-a-sessions-model-from-the-composer.md)) and Send, and Abort while a run worked. On a phone the chip took a row of its own, so the composer was three rows high before a word was typed. Other chat clients draw the same actions as a slim row: icons for attach and send, a small chip for the model.

**A reply could not be copied whole.** The transcript draws a reply as Markdown. Selecting it copies the text as drawn, without its Markdown, and a long reply has to be selected by hand across the list.

**The context fill showed in one place.** [ADR-0052](0052-show-each-sessions-context-fill-and-usage-and-sum-the-days-usage-in-code.md) decision 7 put the meter in the desktop's session header, and on a phone the note under the title said `context 61%`. The owner writes in the composer, at the bottom of the window, while the header is at its top; the iPhone showed the fill only as words.

## Decision

1. **The composer's buttons are slim.** One row under the field holds, in order: Attach files as a paperclip icon button, on a phone Insert a saved prompt as an icon button, the model chip, Sending… while a message goes, then at the end Abort as a stop icon button while a run works, and Send as a compact primary button (its icon and a short label). Each keeps its accessible name, its tooltip, its enabled and disabled states and its action; the field's keys are unchanged. The model chip is a small button with a model icon, the label `state.ChipOf` gives, cut with an ellipsis when the row is narrow, and a chevron; its menu is unchanged. A phone keeps one row: the chip shrinks before any button does.
2. **A finger can press every slim control.** On iOS, the iPad's full window as the iPhone's, each icon button, the chip, Send and the Copy button take at least `state.MinTouchTarget` (44 DIPs) a side (`state.MinTarget`), as Apple's guidelines ask; elsewhere they keep the kit's size.
3. **Each finished reply has a Copy button.** Under an agent's reply, a small copy icon button named Copy puts the reply's Markdown source on the clipboard, its thinking left out (`state.Row.Copy`). Only a finished reply with words has one: not the owner's or a supervisor's message, a tool call, a note, a question or the reply still being written. Once it copied, the button shows a check named Copied for `state.CopyHold` (`state.Copied`), and screen readers hear "Copied the reply"; a failed copy says why in a toast.
4. **The context mark sits under the composer.** A thin row under the composer's box ends with the context mark: a thin bar and the percent (`state.ContextFill.Bar`, `Label`), colored by its level, with the exact numbers in its tooltip, on every layout, a session with no composer (a worker's, a review's) included. The thresholds, the label and the tooltip are ADR-0052's, unchanged.
5. **The header keeps the session's tokens and cost, not the mark.** The desktop's header still shows the session's tokens and cost, and no longer the bar and percent; the phone's note no longer says `context 61%`. One mark, by the field the owner writes in, is enough, and the header's room goes to the title.
6. **No wire change.** Everything the views read was in the desktop already.

## Consequences

- The composer is two rows on every layout, the field and its buttons, with the thin row of the context mark under it once an answer measured the context.
- A reply copied is its Markdown, which pastes as written into an issue, a pull request or a note.
- The context mark shows on the iPhone as a bar for the first time.
- frostui has an icon button but no primary icon button and no chip with a menu; the composer builds Send from `frostui.LabeledButton` and the chip from MyGo's `ButtonBase` with `Menu`. A kit component for both would let other apps share them.
- Retry and a reply's thumbs up or down are not built; they are possible follow-ups.
- The desktop must be rebuilt; pocketd is unchanged.

## Alternatives considered

- **Keep the mark in the header and add the composer's:** the desktop would show the same number twice, a row apart.
- **A footer with the folder and the branch, as other clients show:** the folder is in the header and the phone's note already, and no branch reaches the desktop; a branch would need a protocol change for a line the owner did not ask for.
- **Copy the reply as drawn (plain text):** the Markdown is the reply as the agent wrote it, and pastes into GitHub as it was meant.
- **Show the Copy button on hover alone:** a touch screen has no hover, and the keyboard would find it only by chance.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md#phase-2--team-work-board-and-inbox) (item 38, the slim composer)
- Builds on: [ADR-0052](0052-show-each-sessions-context-fill-and-usage-and-sum-the-days-usage-in-code.md), whose decision 7's header meter and phone note it amends, [ADR-0028](0028-let-the-owner-change-a-sessions-model-from-the-composer.md), [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
