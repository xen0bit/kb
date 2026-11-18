// Package skill carries the agent skill for using kb, embedded so the skill
// installed by `kb skill install` always matches the tools of this binary.
package skill

import _ "embed"

// Content is SKILL.md.
//
//go:embed SKILL.md
var Content []byte
