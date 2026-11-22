// Command harnesshook translates trusted project PreToolUse events to the platform's commit policy.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/tools/harnessdoctor/gitworktree"
)

type args struct {
	Command     string `json:"command"`
	Cmd         string `json:"cmd"`
	Workdir     string `json:"workdir"`
	CommandLine string `json:"CommandLine"`
}

type input struct {
	Tool    string `json:"tool_name"`
	Role    string `json:"agent_type"`
	Session string `json:"session_id"`
	Cwd     string `json:"cwd"`
	// Other Codex tools accept arbitrary JSON, including scalar arguments.
	ToolInput json.RawMessage `json:"tool_input"`
	Call      struct {
		Name string `json:"name"`
		Args args   `json:"args"`
	} `json:"toolCall"`
	Conversation string `json:"conversationId"`
}

type event struct{ tool, role, command, dir, session string }

func (in input) event(host string) event {
	if host == "antigravity" {
		// run_command has no working-directory argument. The client supplies
		// its actual directory in the event envelope.
		return event{tool: in.Call.Name, command: in.Call.Args.CommandLine, dir: in.Cwd, session: in.Conversation}
	}
	var a args
	_ = json.Unmarshal(in.ToolInput, &a)
	cmd := a.Command
	if cmd == "" {
		cmd = a.Cmd
	}
	dir := in.Cwd
	if a.Workdir != "" {
		dir = resolveDir(dir, a.Workdir)
	} else if host == "codex" && shellTool(in.Tool) {
		// A normalized Bash payload need not preserve exec_command's workdir.
		// A session cwd cannot certify its stamp. An absolute git -C still can.
		dir = ""
	}
	return event{tool: in.Tool, role: in.Role, command: cmd, dir: dir, session: in.Session}
}

func shellTool(name string) bool {
	switch name {
	case "Bash", "PowerShell", "exec_command", "shell_command", "run_command":
		return true
	}
	return false
}

func resolveDir(base, child string) string {
	if child == "" {
		return base
	}
	if strings.ContainsAny(child, "$`*?") || child == "-" || strings.HasPrefix(child, "~") {
		return ""
	}
	if filepath.IsAbs(child) {
		return filepath.Clean(child)
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, child)
}

func decision(host, deny string, out io.Writer) error {
	if deny == "" {
		return nil
	} // In particular, {} is not a neutral Antigravity decision.
	if host == "antigravity" {
		return json.NewEncoder(out).Encode(map[string]string{"decision": "deny", "reason": deny})
	}
	return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": deny}})
}

var unsafeSession = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func evidenceOnly(root, role string) (bool, error) {
	// These two roles have an unconditional read-only contract, including
	// while a session is reporting a missing or malformed catalog entry.
	if role == "reviewer" || role == "issue-triage" {
		return true, nil
	}
	var catalog struct {
		Agents []struct {
			Name         string `json:"name"`
			EvidenceOnly bool   `json:"codexEvidenceOnly"`
		} `json:"agents"`
	}
	path := filepath.Join(root, "harness", "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	for _, agent := range catalog.Agents {
		if agent.Name == role {
			return agent.EvidenceOnly, nil
		}
	}
	// Codex may still discover a project role whose canonical entry was
	// removed. Keep that adapter read-only until the reported drift is fixed.
	filename := role + ".toml"
	if !filepath.IsLocal(filename) || strings.ContainsAny(filename, `/\`) {
		return false, nil
	}
	_, err = os.Lstat(filepath.Join(root, ".codex", "agents", filename))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // A separately configured host role.
	}
	return err == nil, err
}

func handle(host string, e event, sourceRoot string, out, diagnostic io.Writer) error {
	if host == "codex" && e.role != "" {
		restricted, err := evidenceOnly(sourceRoot, e.role)
		if err != nil {
			return err
		}
		if restricted {
			switch e.tool {
			case "read_file", "list_dir", "grep_files", "view_image":
				return nil
			default:
				return decision(host, e.role+" accepts file evidence only. Request this evidence from the parent session.", out)
			}
		}
	}
	if !shellTool(e.tool) || os.Getenv("VERIFY_GATE_MODE") == "off" {
		return nil
	}
	session := unsafeSession.ReplaceAllString(e.session, "_")
	if session == "" {
		session = "default"
	}
	if len(session) > 64 {
		session = session[:64]
	}
	policy := filepath.ToSlash(filepath.Join(sourceRoot, "harness", "hooks", "verify-commit-gate.sh"))
	for _, target := range commits(e.command, e.dir) {
		cwd := target.dir
		known := "known"
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			// A command can create the target before invoking Git. Apply the
			// unknown-target and trailer policy from an existing checkout.
			cwd = sourceRoot
			known = "unknown"
		}
		trailer := "0"
		if target.trailer {
			trailer = "1"
		}
		// Git supplies the project's POSIX shell on Windows. Pass all paths and
		// metadata as argv, including when the checkout has a space or apostrophe.
		cmd := exec.Command("git", "-c", "alias.map-gate=!bash", "map-gate", policy, session, trailer, known)
		cmd.Dir = cwd
		cmd.Env = gitworktree.Environment()
		var log bytes.Buffer
		cmd.Stderr = &log
		err := cmd.Run()
		if err != nil {
			var exited *exec.ExitError
			if errors.As(err, &exited) && exited.ExitCode() == 2 {
				return decision(host, strings.TrimSpace(log.String()), out)
			}
			return fmt.Errorf("verification policy in %s: %w: %s", cwd, err, strings.TrimSpace(log.String()))
		}
		if _, err := io.Copy(diagnostic, &log); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "claude" && os.Args[1] != "codex" && os.Args[1] != "antigravity") {
		fmt.Fprintln(os.Stderr, "usage: harnesshook claude|codex|antigravity")
		os.Exit(2)
	}
	host := os.Args[1]
	var in input
	err := json.NewDecoder(os.Stdin).Decode(&in)
	if err == nil {
		var root string
		root, err = os.Getwd()
		if err == nil {
			err = handle(host, in.event(host), root, os.Stdout, os.Stderr)
		}
	}
	if err != nil {
		if e := decision(host, "Could not check platform tool policy: "+err.Error(), os.Stdout); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}
}
