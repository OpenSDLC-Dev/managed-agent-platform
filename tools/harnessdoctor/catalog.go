package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/tools/harnessdoctor/internal/toml"
)

type manifest struct {
	Skills []string `json:"skills"`
	Rules  []rule   `json:"rules"`
	Agents []agent  `json:"agents"`
}
type rule struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
}
type agent struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	ClaudeTools       []string `json:"claudeTools"`
	AntigravityTools  []string `json:"antigravityTools"`
	ClaudeBashGuard   bool     `json:"claudeBashGuard,omitempty"`
	CodexEvidenceOnly bool     `json:"codexEvidenceOnly,omitempty"`
}

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var toolName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func loadManifest(root string) (manifest, error) {
	var m manifest
	const name = "harness/manifest.json"
	raw, err := regularFile(root, name)
	if err != nil {
		return m, err
	}
	if !json.Valid(raw) {
		return m, fmt.Errorf("%s: malformed JSON", name)
	}
	if _, err := jsonObject(raw); err != nil {
		return m, fmt.Errorf("%s: %w", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %w", name, err)
	}
	skills := slices.Clone(m.Skills)
	rules := make([]string, 0, len(m.Rules))
	agents := make([]string, 0, len(m.Agents))
	for _, r := range m.Rules {
		if len(r.Paths) == 0 {
			return m, fmt.Errorf("%s: rule %s has no path patterns", name, r.Name)
		}
		for _, p := range r.Paths {
			if _, err := compileGlob(p); err != nil {
				return m, fmt.Errorf("%s: %w", name, err)
			}
		}
		rules = append(rules, r.Name+".md")
	}
	for _, a := range m.Agents {
		if strings.TrimSpace(a.Description) == "" || len(a.ClaudeTools) == 0 || len(a.AntigravityTools) == 0 {
			return m, fmt.Errorf("%s: agent %s needs a description and native tool lists", name, a.Name)
		}
		if a.ClaudeBashGuard && !slices.Contains(a.ClaudeTools, "Bash") {
			return m, fmt.Errorf("%s: agent %s has a Bash guard without the Bash tool", name, a.Name)
		}
		for _, list := range [][]string{a.ClaudeTools, a.AntigravityTools} {
			for _, nativeTool := range list {
				if !toolName.MatchString(nativeTool) {
					return m, fmt.Errorf("%s: invalid tool name %q for %s", name, nativeTool, a.Name)
				}
			}
		}
		if a.Name == "verifier" && (a.CodexEvidenceOnly || !slices.Contains(a.ClaudeTools, "Bash") || !slices.Contains(a.AntigravityTools, "run_command")) {
			return m, fmt.Errorf("%s: verifier needs a command tool to run its checks on each host", name)
		}
		if a.Name == "issue-triage" || a.Name == "reviewer" {
			if !a.CodexEvidenceOnly {
				return m, fmt.Errorf("%s: %s needs an evidence-only Codex role", name, a.Name)
			}
			for _, t := range a.AntigravityTools {
				if t != "read_file" {
					return m, fmt.Errorf("%s: %s must use a read-only Antigravity tool, got %s", name, a.Name, t)
				}
			}
			for _, t := range a.ClaudeTools {
				if t != "Read" && t != "Grep" && t != "Glob" && !(t == "Bash" && a.ClaudeBashGuard && a.Name == "issue-triage") {
					return m, fmt.Errorf("%s: %s must use read-only Claude tools, got %s", name, a.Name, t)
				}
			}
		}
		agents = append(agents, a.Name+".md")
	}
	for _, role := range []string{"issue-triage", "reviewer", "verifier"} {
		if !slices.Contains(agents, role+".md") {
			return m, fmt.Errorf("%s: register the %s role required by the platform workflow", name, role)
		}
	}
	for _, dir := range []struct {
		name  string
		files []string
		ext   string
	}{{"skills", skills, ""}, {"rules", rules, ".md"}, {"agents", agents, ".md"}} {
		if len(dir.files) == 0 {
			return m, fmt.Errorf("%s: empty %s catalog", name, dir.name)
		}
		for _, file := range dir.files {
			if !slug.MatchString(strings.TrimSuffix(file, dir.ext)) {
				return m, fmt.Errorf("%s: invalid %s name %q", name, dir.name, file)
			}
		}
		slices.Sort(dir.files)
		if len(slices.Compact(slices.Clone(dir.files))) != len(dir.files) {
			return m, fmt.Errorf("%s: duplicate %s entry", name, dir.name)
		}
		folder := "harness/" + dir.name
		p, err := safePath(root, folder)
		if err != nil {
			return m, err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return m, err
		}
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.Name())
		}
		if !slices.Equal(got, dir.files) {
			return m, fmt.Errorf("%s: entries %v differ from %s %v", folder, got, name, dir.files)
		}
	}
	return m, nil
}

// Globs use repository-relative slashes. A whole ** directory also matches zero directories.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if !fs.ValidPath(pattern) || pattern == "." || strings.ContainsAny(pattern, `\:,{}[]`) {
		return nil, fmt.Errorf("invalid rule path pattern %q", pattern)
	}
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:[^/]+/)*")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			n := strings.IndexAny(pattern[i:], "*?")
			if n < 0 {
				n = len(pattern) - i
			}
			b.WriteString(regexp.QuoteMeta(pattern[i : i+n]))
			i += n
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

// Return an existing directory for Git and the remaining suffix, which may
// include planned directories and files. Follow a file symlink to its target.
func ruleDirectory(name string) (string, string, error) {
	full, err := filepath.EvalSymlinks(name)
	if err == nil {
		info, err := os.Stat(full)
		if err != nil {
			return "", "", err
		}
		if info.IsDir() {
			return full, "", nil
		}
		return filepath.Dir(full), filepath.Base(full), nil
	}
	if !os.IsNotExist(err) || filepath.Dir(name) == name {
		return "", "", err
	}
	dir, tail, err := ruleDirectory(filepath.Dir(name))
	return dir, filepath.Join(tail, filepath.Base(name)), err
}

func matchingRules(root, cwd string, args []string, m manifest) ([]string, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	prefixes := map[string]string{}
	names := make([]string, 0, len(args))
	for _, arg := range args {
		name := filepath.FromSlash(strings.ReplaceAll(arg, `\`, "/"))
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		dir, tail, err := ruleDirectory(name)
		if err != nil {
			return nil, err
		}
		prefix, known := prefixes[dir]
		if !known {
			// Git for Windows resolves directory junctions that EvalSymlinks
			// can keep in their original spelling. Compare worktree roots, not
			// common Git directories shared by sibling linked checkouts.
			tree, err := gitWorktreePath(dir, "--show-toplevel")
			if err != nil {
				return nil, fmt.Errorf("path outside current worktree: %s (%w)", arg, err)
			}
			other, err := os.Stat(tree)
			if err != nil {
				return nil, err
			}
			if !os.SameFile(rootInfo, other) {
				return nil, fmt.Errorf("path outside current worktree: %s", arg)
			}
			prefix, err = gitWorktreePath(dir, "--show-prefix")
			if err != nil {
				return nil, err
			}
			prefixes[dir] = prefix
		}
		names = append(names, filepath.ToSlash(filepath.Join(prefix, tail)))
	}
	var result []string
	for _, r := range m.Rules {
		for _, p := range r.Paths {
			g, err := compileGlob(p)
			if err != nil {
				return nil, err
			}
			if slices.ContainsFunc(names, g.MatchString) {
				result = append(result, "harness/rules/"+r.Name+".md")
				break
			}
		}
	}
	slices.Sort(result)
	return result, nil
}

func literal(value any) string {
	// JSON strings and lists are valid YAML frontmatter values.
	b, _ := json.Marshal(value)
	return string(b)
}

type output struct {
	data   []byte
	source string // A copied skill attachment takes its executable bit from the source.
}

func roleText(a agent, host, body string) (string, error) {
	if host == "codex" {
		definition := struct {
			Name         string `toml:"name"`
			Description  string `toml:"description"`
			Instructions string `toml:"developer_instructions"`
		}{a.Name, a.Description, body}
		encoded, err := toml.Marshal(definition)
		if err != nil {
			return "", err
		}
		text := string(encoded)
		if a.CodexEvidenceOnly {
			text += "\n[features]\nshell_tool = false\napps = false\nplugins = false\nrequest_permissions_tool = false\n"
		}
		return text, nil
	}
	meta := "---\nname: " + a.Name + "\ndescription: " + literal(a.Description) + "\nmodel: inherit\n"
	if host == "claude" {
		meta += "tools: " + strings.Join(a.ClaudeTools, ", ") + "\n"
		if a.ClaudeBashGuard {
			meta += "hooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: " + literal(`"$CLAUDE_PROJECT_DIR"/harness/hooks/issue-triage-bash-guard.sh`) + "\n"
		}
	} else {
		meta += "tools: " + literal(a.AntigravityTools) + "\nmainAgent: false\nsubagent: true\n"
	}
	return meta + "---\n\n" + body, nil
}

func cleanText(root, name string) (string, error) {
	data, err := regularFile(root, name)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("%s must contain UTF-8 text", name)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}

func skillHeader(name string, data []byte) error {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	head, body, found := strings.Cut(strings.TrimPrefix(s, "---\n"), "\n---\n")
	if !strings.HasPrefix(s, "---\n") || !found {
		return errors.New("missing skill frontmatter")
	}
	if !utf8.Valid(data) || strings.TrimSpace(body) == "" {
		return errors.New("a skill needs a nonempty UTF-8 body")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || len(line) == 0 || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key = strings.TrimSpace(key)
		if strings.HasPrefix(key, "\"") {
			decoded, err := strconv.Unquote(key)
			if err != nil {
				return fmt.Errorf("invalid quoted skill frontmatter key %q", key)
			}
			key = decoded
		} else if len(key) >= 2 && key[0] == '\'' && key[len(key)-1] == '\'' {
			key = strings.ReplaceAll(key[1:len(key)-1], "''", "'")
		}
		if _, duplicate := fields[key]; duplicate {
			return fmt.Errorf("duplicate skill frontmatter key %q", key)
		}
		fields[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if fields["name"] != name || fields["description"] == "" {
		return fmt.Errorf("expected skill name %q and a description", name)
	}
	for _, key := range []string{"model", "effort", "model_reasoning_effort"} {
		if _, ok := fields[key]; ok {
			return fmt.Errorf("remove the %s override so the skill inherits its session", key)
		}
	}
	return nil
}

func render(root, host string, m manifest) (map[string]output, error) {
	result := map[string]output{}
	// These runtime dependencies are shared by all three PreToolUse adapters.
	for _, path := range []string{"tools/harnesshook/main.go", "tools/harnesshook/command.go", "harness/hooks/verify-commit-gate.sh", "harness/hooks/verify-stamp.sh"} {
		if _, err := regularFile(root, path); err != nil {
			return nil, err
		}
	}
	for _, name := range m.Skills {
		prefix := "harness/skills/" + name
		files, err := walkFiles(root, prefix)
		if err != nil {
			return nil, err
		}
		entry := prefix + "/SKILL.md"
		if !slices.Contains(files, entry) {
			return nil, fmt.Errorf("missing %s", entry)
		}
		for _, src := range files {
			b, err := regularFile(root, src)
			if err != nil {
				return nil, err
			}
			if src == entry {
				if err := skillHeader(name, b); err != nil {
					return nil, fmt.Errorf("%s: %w", src, err)
				}
			}
			if strings.HasSuffix(src, ".md") {
				if err := checkLinks(root, src, string(b)); err != nil {
					return nil, err
				}
			}
			rel := strings.TrimPrefix(src, "harness/")
			if host == "" || host == "claude" {
				result[".claude/"+rel] = output{b, src}
			}
			if host != "claude" {
				result[".agents/"+rel] = output{b, src}
			}
		}
	}
	for _, r := range m.Rules {
		name := "rules/" + r.Name + ".md"
		body, err := cleanText(root, "harness/"+name)
		if err != nil {
			return nil, err
		}
		if err := checkLinks(root, "harness/"+name, body); err != nil {
			return nil, err
		}
		if host == "" || host == "claude" {
			result[".claude/"+name] = output{data: []byte("---\npaths: " + literal(r.Paths) + "\n---\n\n" + body)}
		}
		if host == "" || host == "antigravity" {
			result[".agents/"+name] = output{data: []byte("---\ntrigger: glob\nglobs: " + literal(strings.Join(r.Paths, ",")) + "\n---\n\n" + body)}
		}
	}
	for _, a := range m.Agents {
		name := "agents/" + a.Name
		body, err := cleanText(root, "harness/"+name+".md")
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(body, "---\n") {
			return nil, fmt.Errorf("harness/%s.md: role frontmatter belongs in the manifest", name)
		}
		if err := checkLinks(root, "harness/"+name+".md", body); err != nil {
			return nil, err
		}
		if a.ClaudeBashGuard && (host == "" || host == "claude") {
			for _, path := range []string{"harness/hooks/issue-triage-bash-guard.sh", "harness/hooks/issue-triage-read.sh"} {
				if _, err := regularFile(root, path); err != nil {
					return nil, fmt.Errorf("role %s: %w", a.Name, err)
				}
			}
		}
		for _, client := range hosts {
			if host != "" && client != host {
				continue
			}
			path := map[string]string{"claude": ".claude/" + name + ".md", "antigravity": ".agents/" + name + ".md", "codex": ".codex/" + name + ".toml"}[client]
			text, err := roleText(a, client, body)
			if err != nil {
				return nil, fmt.Errorf("%s: cannot encode %s role: %w", path, a.Name, err)
			}
			result[path] = output{data: []byte(text)}
		}
	}
	return result, nil
}
