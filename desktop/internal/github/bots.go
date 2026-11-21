package github

import "strings"

// dependencyBots are the GitHub Apps whose risk:high pull requests Merge
// All Ready lists without the diff opened (pocketgo ADR-0031, the owner's
// choice of 2026-11-21): Dependabot and Renovate, by the login GitHub's
// GraphQL gives their Bot accounts, the App's slug, which no other account
// has. An App the agents act through is a Bot too, and is not on the list:
// its pull requests need the diff opened as a person's do.
var dependencyBots = []string{"dependabot", "renovate"}

// DependencyBot says an author GitHub's GraphQL names by its __typename and
// login is a dependency bot of the list: a Bot, never a User or another
// type, whose login is one of the list, in any case, as GitHub compares
// logins.
func DependencyBot(authorType, login string) bool {
	if authorType != "Bot" {
		return false
	}
	for _, bot := range dependencyBots {
		if strings.EqualFold(login, bot) {
			return true
		}
	}
	return false
}
