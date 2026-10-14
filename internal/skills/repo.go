package skills

import (
	"strings"
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
// cannot hold, and is cut to the upload cap of 1,024 characters (ours: the
// reference's own bound is unrecorded).
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
	if utf8.RuneCountInString(d) > maxDescriptionLen {
		d = string([]rune(d)[:maxDescriptionLen])
	}
	return d
}
