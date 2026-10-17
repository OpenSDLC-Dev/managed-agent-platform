package skills

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// RepoSkill is one skill found in a mounted repository, as the brain announces
// it (plan 65 decision 3). sessions.repo_skills maps each scanned repository
// resource's id to its skills: internal/executor writes it, internal/brain
// reads it.
type RepoSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

// RepoDescription is the description of a skill found in a mounted
// repository's .claude/skills directory (plan 65 decision 3). Upload holds a
// SKILL.md to the rules parseFrontmatter enforces; a repository's is what was
// committed, so this reads leniently, as the reference was recorded doing: a
// SKILL.md with no frontmatter, frontmatter that is not valid YAML, or no
// description is listed with an empty description rather than left out. YAML
// admits only valid UTF-8; what it reads loses its NULs, which a jsonb value
// cannot hold, and is cut to maxRepoDescriptionLen characters.
func RepoDescription(md []byte) string {
	body, ok := frontmatterBlock(md)
	if !ok {
		return ""
	}
	var fm struct {
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal(body, &fm) != nil {
		return ""
	}
	d := strings.ReplaceAll(fm.Description, "\x00", "")
	if utf8.RuneCountInString(d) > maxRepoDescriptionLen {
		d = string([]rune(d)[:maxRepoDescriptionLen])
	}
	return d
}

// maxRepoDescriptionLen is where a repository skill's description is cut. The
// reference was recorded announcing descriptions of 500, 1,000 and 2,000
// characters whole (managed-agents-wire-recordings 2026-10-15-repo-followups,
// setup[51]); its bound above that is unrecorded, so the longest one seen
// whole is ours.
const maxRepoDescriptionLen = 2000

// RepoName is the name a repository skill's SKILL.md frontmatter gives it, as
// the reference was recorded announcing a skill under its frontmatter's name
// rather than its directory's (2026-10-15-repo-followups, setup[14]). It is ""
// where the frontmatter gives none usable: no frontmatter or no name, a name
// YAML cannot read as a string, an empty one, one longer than the upload cap
// of 64 bytes, or one holding a control character, which would break the list
// it is announced in. The caller then takes the directory's name (ours: what
// the reference does there is unrecorded). The name is read on its own, so a
// description YAML cannot read leaves it, and the other way round.
func RepoName(md []byte) string {
	body, ok := frontmatterBlock(md)
	if !ok {
		return ""
	}
	var fm struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal(body, &fm) != nil || len(fm.Name) > maxNameLen ||
		strings.ContainsFunc(fm.Name, unicode.IsControl) {
		return ""
	}
	return fm.Name
}
