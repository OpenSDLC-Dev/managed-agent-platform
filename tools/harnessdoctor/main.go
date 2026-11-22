// Command harnessdoctor checks the current worktree's coding-agent adapters offline.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

const command = "go run ./tools/harnessdoctor"
const usage = command + " [check [--host claude|codex|antigravity] | sync | rules <paths...> | session-start <host>]"

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(run(cwd, os.Args[1:], os.Stdout))
}

func run(cwd string, args []string, out io.Writer) int {
	if len(args) == 0 {
		args = []string{"check"}
	}
	action := args[0]
	host := ""
	switch {
	case action == "check" && len(args) == 3 && args[1] == "--host" && slices.Contains(hosts, args[2]):
		host = args[2]
	case action == "session-start" && len(args) == 2 && slices.Contains(hosts, args[1]):
		host = args[1]
	case (action == "check" || action == "sync") && len(args) == 1:
	case action == "rules" && len(args) > 1:
	default:
		fmt.Fprintln(out, usage)
		return 2
	}
	root, err := worktree(cwd)
	var m manifest
	if err == nil {
		m, err = loadManifest(root)
	}
	if err == nil && action == "rules" {
		var matches []string
		matches, err = matchingRules(root, cwd, args[1:], m)
		if err == nil {
			var rules strings.Builder
			for _, name := range matches {
				body, readErr := regularFile(root, name)
				if readErr != nil {
					err = readErr
					break
				}
				fmt.Fprintf(&rules, "%s\n%s\n", name, strings.TrimRight(string(body), "\r\n"))
			}
			if err == nil {
				fmt.Fprint(out, rules.String())
				return 0
			}
		}
	}
	if err == nil {
		err = checkGuide(root)
	}
	var problems []string
	if err == nil {
		var files map[string]output
		files, err = render(root, host, m)
		if err == nil {
			changes, conflicts, modeFixes := compare(root, host, files)
			problems = append(problems, conflicts...)
			for _, c := range changes {
				if action == "sync" && len(conflicts) == 0 {
					if err := c.apply(root); err != nil {
						problems = append(problems, err.Error())
						break
					}
				} else {
					problems = append(problems, c.name+": missing or differs from its source; run "+command+" sync")
				}
			}
			problems = append(problems, modeFixes...)
		}
	}
	if err != nil {
		problems = append(problems, err.Error())
	}
	target := root
	if target == "" {
		target = cwd
	}
	message := "harness-doctor: OK (" + target + ")"
	if len(problems) > 0 {
		message = "harness-doctor: FAIL (" + target + ")\n" + strings.Join(problems, "\n") + "\nCorrect the reported source or conflict, then run " + command + " sync and " + command + " check."
	}
	if action == "session-start" {
		if host == "claude" {
			message += "\nIf this worktree's AGENTS.md did not load, read it before working."
		}
		var response any
		if host == "antigravity" {
			response = map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": message}}}}
		} else {
			response = map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": message}}
		}
		if err := json.NewEncoder(out).Encode(response); err != nil {
			return 1
		}
		// Returning a nonzero hook exit can make the host drop its diagnostic.
		// The check subcommand supplies the machine-readable failure exit code.
		return 0
	}
	fmt.Fprintln(out, message)
	if len(problems) > 0 {
		return 1
	}
	return 0
}
