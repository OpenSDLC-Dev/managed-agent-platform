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
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
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

// Git or the client may supply a Windows short path on either side of a
// comparison. A planned file can be absent, so expand its existing parent.
func rulePath(name string) (string, error) {
	if runtime.GOOS != "windows" {
		return name, nil
	}
	full, err := filepath.EvalSymlinks(name)
	if err == nil || !os.IsNotExist(err) || filepath.Dir(name) == name {
		return full, err
	}
	parent, err := rulePath(filepath.Dir(name))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(name)), nil
}

func matchingRules(root, cwd string, args []string, m manifest) ([]string, error) {
	root, err := rulePath(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(args))
	for _, arg := range args {
		name := filepath.FromSlash(strings.ReplaceAll(arg, `\`, "/"))
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		name, err = rulePath(name)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("path outside current worktree: %s", arg)
		}
		names = append(names, filepath.ToSlash(rel))
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
	// All callers supply strings or lists of strings. JSON string escapes are also
	// valid in YAML double-quoted scalars and TOML basic strings.
	b, _ := json.Marshal(value)
	return string(b)
}

type output struct {
	data   []byte
	source string // A copied skill attachment takes its executable bit from the source.
}

func roleText(a agent, host, body string) string {
	if host == "codex" {
		text := "name = " + literal(a.Name) + "\ndescription = " + literal(a.Description) + "\ndeveloper_instructions = " + literal(body) + "\n"
		if a.CodexEvidenceOnly {
			text += "\n[features]\nshell_tool = false\napps = false\nplugins = false\nrequest_permissions_tool = false\n"
		}
		return text
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
	return meta + "---\n\n" + body
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
		if ok && strings.TrimSpace(key) == key {
			fields[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
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
	for _, path := range []string{"tools/harnesshook/main.go", "harness/hooks/verify-commit-gate.sh", "harness/hooks/verify-stamp.sh"} {
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
			paths := make([]string, 0, len(r.Paths))
			for _, p := range r.Paths {
				if !strings.HasPrefix(p, "**/") {
					p = "**/" + p
				}
				paths = append(paths, p)
			}
			result[".agents/"+name] = output{data: []byte("---\ntrigger: glob\nglobs: " + literal(strings.Join(paths, ",")) + "\n---\n\n" + body)}
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
			result[path] = output{data: []byte(roleText(a, client, body))}
		}
	}
	return result, nil
}
