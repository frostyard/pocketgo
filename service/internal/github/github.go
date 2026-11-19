// Package github reads a pull request on GitHub through gh, run in the
// sandbox with the service user's sign-in (ADR-0016), and checks what it
// reads field by field: nothing it returns is taken on an agent's word.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// GH runs gh with args and returns what it printed, or an error that says
// why it failed. pocketd runs it in the sandbox (Engine.GH).
type GH func(ctx context.Context, args ...string) (string, error)

// States of a pull request.
const (
	Open   = "open"
	Closed = "closed"
	Merged = "merged"
)

// MaxTitle is the most characters of a pull request's title pocketd keeps.
const MaxTitle = 300

// Pull is a pull request as GitHub has it.
type Pull struct {
	// ID is GitHub's own id of the pull request, which no rename of its
	// repository reuses.
	ID int64
	// Repository is owner/name as GitHub spells it; Number its number.
	Repository string
	Number     int64
	Title      string
	URL        string
	State      string
	Draft      bool
	// Head is the branch it comes from, in HeadRepository ("" when that
	// repository is gone), at commit HeadSHA; Base the branch it merges
	// into.
	Head           string
	HeadRepository string
	HeadSHA        string
	Base           string
	// Labels are its labels' names.
	Labels []string
}

// owner and name are GitHub's account and repository names: an account
// starts with a letter or digit, and a repository is never . or ..
var (
	owner = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	name  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// CheckRepository refuses a repository that is not owner/name.
func CheckRepository(repository string) error {
	who, repo, ok := strings.Cut(repository, "/")
	if !ok || !owner.MatchString(who) || !name.MatchString(repo) || repo == "." || repo == ".." {
		return fmt.Errorf("%q is not a repository: give it as owner/name", repository)
	}
	return nil
}

// Read reads pull request number of repository on GitHub.
func Read(ctx context.Context, gh GH, repository string, number int64) (Pull, error) {
	if err := CheckRepository(repository); err != nil {
		return Pull{}, err
	}
	if number <= 0 {
		return Pull{}, fmt.Errorf("%d is not a pull request number", number)
	}
	out, err := gh(ctx, "api", "--hostname", "github.com", "repos/"+repository+"/pulls/"+strconv.FormatInt(number, 10))
	if err != nil {
		return Pull{}, fmt.Errorf("read %s#%d on GitHub: %w", repository, number, err)
	}
	return parse(repository, number, out)
}

// answer is the part of GitHub's pull request that pocketd reads.
type answer struct {
	ID       int64           `json:"id"`
	Number   int64           `json:"number"`
	Title    json.RawMessage `json:"title"`
	HTMLURL  string          `json:"html_url"`
	State    string          `json:"state"`
	Draft    json.RawMessage `json:"draft"`
	MergedAt json.RawMessage `json:"merged_at"`
	Labels   json.RawMessage `json:"labels"`
	Head     struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// parse checks GitHub's answer for pull request number of repository.
func parse(repository string, number int64, out string) (Pull, error) {
	var a answer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		return Pull{}, fmt.Errorf("GitHub's answer for %s#%d is not a pull request: %w", repository, number, err)
	}
	full := a.Base.Repo.FullName
	switch {
	case a.ID <= 0:
		return Pull{}, fmt.Errorf("GitHub's answer for %s#%d has no id", repository, number)
	case a.Number != number:
		return Pull{}, fmt.Errorf("GitHub answered for #%d, not #%d", a.Number, number)
	case !strings.EqualFold(full, repository) || CheckRepository(full) != nil:
		return Pull{}, fmt.Errorf("GitHub answered for %q, not %s", full, repository)
	case a.HTMLURL != "https://github.com/"+full+"/pull/"+strconv.FormatInt(number, 10):
		return Pull{}, fmt.Errorf("GitHub's link for %s#%d is %q", repository, number, a.HTMLURL)
	case a.Head.Ref == "" || a.Base.Ref == "":
		return Pull{}, errors.New("GitHub's answer names no head or base branch")
	case !commitSHA.MatchString(a.Head.SHA):
		return Pull{}, fmt.Errorf("GitHub's head commit %q is not a commit", a.Head.SHA)
	}
	// The title and draft must be there, as a string and a boolean: a
	// missing or null one is no answer, never an empty title or false.
	var title string
	var draft bool
	if err := strict(a.Title, &title); err != nil {
		return Pull{}, fmt.Errorf("GitHub's title for %s#%d: %w", repository, number, err)
	}
	if err := strict(a.Draft, &draft); err != nil {
		return Pull{}, fmt.Errorf("GitHub's draft for %s#%d: %w", repository, number, err)
	}
	// The labels must be there, as a list of named labels: pocketd keeps
	// ready-for-human in step with them (ADR-0017).
	var labels []struct {
		Name string `json:"name"`
	}
	if err := strict(a.Labels, &labels); err != nil {
		return Pull{}, fmt.Errorf("GitHub's labels for %s#%d: %w", repository, number, err)
	}
	p := Pull{ID: a.ID, Repository: full, Number: number, Title: clip(strings.TrimSpace(title)), URL: a.HTMLURL, Draft: draft,
		Head: a.Head.Ref, HeadSHA: a.Head.SHA, Base: a.Base.Ref}
	for _, l := range labels {
		if l.Name == "" {
			return Pull{}, fmt.Errorf("GitHub's labels for %s#%d hold one with no name", repository, number)
		}
		p.Labels = append(p.Labels, l.Name)
	}
	if a.Head.Repo != nil {
		p.HeadRepository = a.Head.Repo.FullName
	}
	merged, err := mergedAt(a.MergedAt)
	if err != nil {
		return Pull{}, err
	}
	switch {
	case a.State == "open" && !merged:
		p.State = Open
	case a.State == "closed" && merged:
		p.State = Merged
	case a.State == "closed":
		p.State = Closed
	default:
		return Pull{}, fmt.Errorf("GitHub's state %q, merged %v, is not a pull request's", a.State, merged)
	}
	return p, nil
}

// strict decodes raw into v, which raw must hold: present, not null, and
// of v's type.
func strict(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("missing")
	}
	return json.Unmarshal(raw, v)
}

// mergedAt says whether GitHub's merged_at says merged: it is present,
// and null or a time; anything else is no answer, so a malformed one
// never closes a thread.
func mergedAt(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("GitHub's answer has no merged_at")
	}
	if string(raw) == "null" {
		return false, nil
	}
	var at string
	if err := json.Unmarshal(raw, &at); err != nil {
		return false, fmt.Errorf("GitHub's merged_at %s is not a time", raw)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		return false, fmt.Errorf("GitHub's merged_at %q is not a time", at)
	}
	return true, nil
}

// clip keeps the first MaxTitle characters of a title.
func clip(text string) string {
	if utf8.RuneCountInString(text) <= MaxTitle {
		return text
	}
	return string([]rune(text)[:MaxTitle-1]) + "…"
}
