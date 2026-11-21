package github

// CheckIntent holds a read to what the owner confirmed: the head, the base
// branch, the method and the queue flag of the preview, and the rule for
// risk:high (never in a batch; alone, only after the diff at the shown head).
func CheckIntent(in Intent, f PRFacts, r RepoRules) (ok bool, reason string) {
	p := in.Preview
	if f.ID != p.NodeID {
		return false, "this is not the PR you confirmed"
	}
	if f.HeadRefOid != p.Head {
		return false, "new commit since you confirmed (" + shortSHA(f.HeadRefOid) + ")"
	}
	if f.BaseRefName != p.BaseRef {
		return false, "the base branch changed to " + f.BaseRefName
	}
	if queued(f, r) != p.Queue {
		if p.Queue {
			return false, "the merge queue is off now"
		}
		return false, "the base has a merge queue now"
	}
	if method, _ := mergeMethod(f, r); method != p.Method {
		return false, "the merge method changed"
	}
	risk := f.hasLabel(labelRisk)
	if risk && !p.RiskHigh {
		return false, labelRisk + " was added since you confirmed"
	}
	switch in.Mode {
	case "batch":
		if risk {
			return false, labelRisk + " is never in a batch"
		}
	case "single":
		// A preview of risk:high binds the merge to the diff the owner
		// opened, also when the label went since (pocketgo ADR-0018, Codex
		// review of #24, round 6).
		risk := risk || p.RiskHigh
		// Merge All Ready's list merges a dependency bot's risk:high pull
		// request without the diff opened: only while the preview and this
		// read both find its author one of the list (pocketgo ADR-0031, the
		// owner's choice of 2026-11-21). The waiver is of the diff alone:
		// the base stays bound to the one the check read (Codex review of
		// #53, round 5).
		waived := in.WaiveDiff && p.DependencyBot && DependencyBot(f.AuthorType, f.AuthorLogin)
		if risk && !waived && (in.AckHead == "" || in.AckHead != p.Head) {
			return false, "open the diff at " + shortSHA(p.Head) + " first"
		}
		// A waived merge binds to the base commit its check read: one GitHub
		// does not name binds nothing (Codex review of #53, round 6).
		if risk && waived && !isOID(p.BaseOid) {
			return false, "GitHub names no base commit for its check: check it again"
		}
		of := "the diff you opened"
		since := "since you opened the diff"
		if waived {
			of, since = "its check", "since it was checked"
		}
		// The diff the owner opened compares the base commit to the head:
		// a base that moved since is another change (pocketgo ADR-0018).
		if risk && f.BaseRefOid != p.BaseOid {
			return false, "the base moved " + since + " (" + shortSHA(f.BaseRefOid) + "): check it again"
		}
		// The branch read with the rules, after the pull request, must
		// agree: a base that moved between the two reads is another diff
		// (Codex review of #24, round 5).
		if risk && r.baseTip != p.BaseOid {
			return false, "the base branch is at " + shortSHA(r.baseTip) + ", not the base of " + of + ": check it again"
		}
	default:
		return false, "unknown mode " + in.Mode
	}
	return true, ""
}
