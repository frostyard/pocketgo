package team

import "strings"

// OwnerKeyPrefix begins the request key of every request a device makes
// (api's requestKey): a submission of such a key is the owner's own
// message. pocketd's own messages, a schedule's, a note's, a report's,
// a brief's, an answer's or a supervisor's, never carry one (ADR-0065).
const OwnerKeyPrefix = "u:"

// IsOwnerKey says whether a submission of request key is the owner's own
// message.
func IsOwnerKey(key string) bool { return strings.HasPrefix(key, OwnerKeyPrefix) }
