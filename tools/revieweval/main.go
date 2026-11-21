package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "REVIEW.md")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate REVIEW.md above %s", dir)
		}
		dir = parent
	}
}

func filterSubset(cases []Case, subset string, limit int) []Case {
	out := make([]Case, 0, len(cases))
	for _, c := range cases {
		if subset != "all" && c.Subset != subset {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func main() {
	live := flag.Bool("live", false, "run live two-stage LLM reviews against configured endpoint (REVIEWEVAL_* or .env MODEL_*)")
	dryRun := flag.Bool("dry-run", true, "validate dataset, REVIEW.md triggers, and scorer offline without calling any model")
	casesPath := flag.String("cases", "", "optional path to a custom cases.json manifest")
	subset := flag.String("subset", "all", "subset to evaluate: all, tuning, or eval")
	limit := flag.Int("limit", 0, "optional cap on number of cases to run")
	maxDiffBytes := flag.Int("max-diff-bytes", 18000, "maximum diff bytes sent per case in live mode")
	outPath := flag.String("out", "", "optional path to write JSON report")
	flag.Parse()

	if *live {
		*dryRun = false
	}
	if *subset != "all" && *subset != "tuning" && *subset != "eval" {
		fmt.Fprintf(os.Stderr, "revieweval: invalid -subset %q (want all, tuning, or eval)\n", *subset)
		os.Exit(2)
	}

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: %v\n", err)
		os.Exit(1)
	}
	reviewRaw, err := os.ReadFile(filepath.Join(root, "REVIEW.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: read REVIEW.md: %v\n", err)
		os.Exit(1)
	}
	rules := ParseReviewRules(string(reviewRaw))

	allCases, err := LoadCases(*casesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: %v\n", err)
		os.Exit(1)
	}
	if err := ValidateDataset(allCases, rules); err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: dataset invalid: %v\n", err)
		os.Exit(1)
	}

	selected := filterSubset(allCases, *subset, *limit)
	outputs := map[string]StageOutput{}
	triggered := map[string][]string{}

	if *dryRun {
		checkedCommits := 0
		for _, c := range selected {
			paths, _, ok, err := GitDiffForCase(root, c, *maxDiffBytes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "revieweval: case %s: %v\n", c.ID, err)
				os.Exit(1)
			}
			if !ok {
				continue
			}
			checkedCommits++
			matched := MatchRules(rules, paths)
			ids := make([]string, 0, len(matched))
			idSet := map[string]bool{}
			for _, r := range matched {
				ids = append(ids, r.ID)
				idSet[r.ID] = true
			}
			triggered[c.ID] = ids
			for _, want := range c.ExpectedRules {
				if !idSet[want] {
					fmt.Fprintf(os.Stderr, "revieweval: case %s expected REVIEW.md rule %s to trigger on %v (triggered: %v)\n",
						c.ID, want, paths, ids)
					os.Exit(1)
				}
			}
		}
		rep := ScoreRun(selected, outputs, triggered)
		rep.Mode = "dry-run"
		rep.Subset = *subset
		fmt.Printf("revieweval dry-run: ok — %d cases (%d commits verified in local git history, %d REVIEW.md rules)\n",
			len(selected), checkedCommits, len(rules))
		writeReport(rep, *outPath)
		return
	}

	cfg, err := LoadEndpointConfig(root, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: live mode configuration error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("revieweval live: endpoint=%s subset=%s cases=%d\n", cfg, *subset, len(selected))

	client := &LLMClient{Cfg: cfg}
	ctx := context.Background()
	for _, c := range selected {
		paths, diff, ok, err := GitDiffForCase(root, c, *maxDiffBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "revieweval: git diff for %s: %v\n", c.ID, err)
			os.Exit(1)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "revieweval: commit %s for case %s not present in local git history (clone needs full history)\n", c.Commit, c.ID)
			os.Exit(1)
		}
		matched := MatchRules(rules, paths)
		ids := make([]string, 0, len(matched))
		for _, r := range matched {
			ids = append(ids, r.ID)
		}
		triggered[c.ID] = ids

		fmt.Printf("  evaluating %-42s [%s/%s, triggered=%s] ... ",
			c.ID, c.Subset, c.Kind, strings.Join(ids, ","))
		stageOut, err := RunTwoStageReview(ctx, client, matched, diff)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			os.Exit(1)
		}
		outputs[c.ID] = stageOut
		fmt.Printf("raw=%d post_verify=%d final=%d\n", len(stageOut.Raw), len(stageOut.PostVerify), len(stageOut.Final))
	}

	rep := ScoreRun(selected, outputs, triggered)
	rep.Mode = "live"
	rep.Subset = *subset
	rep.Model = cfg.Model

	fmt.Printf("\n=== revieweval summary (%s) ===\n", rep.Model)
	fmt.Printf("Raw:         recall=%d/%d (%.1f%%)  severity_cal=%.1f%%  control_fp=%d/%d (%.1f%%)\n",
		rep.Raw.IncidentsCaught, rep.Raw.IncidentsTotal, rep.Raw.IncidentRecall*100,
		rep.Raw.SeverityCalibration*100,
		rep.Raw.ControlsFalsePositive, rep.Raw.ControlsTotal, rep.Raw.FalsePositiveRate*100)
	fmt.Printf("Post-Verify: recall=%d/%d (%.1f%%)  severity_cal=%.1f%%  control_fp=%d/%d (%.1f%%)\n",
		rep.PostVerify.IncidentsCaught, rep.PostVerify.IncidentsTotal, rep.PostVerify.IncidentRecall*100,
		rep.PostVerify.SeverityCalibration*100,
		rep.PostVerify.ControlsFalsePositive, rep.PostVerify.ControlsTotal, rep.PostVerify.FalsePositiveRate*100)
	fmt.Printf("Final:       recall=%d/%d (%.1f%%)  severity_cal=%.1f%%  control_fp=%d/%d (%.1f%%)\n",
		rep.Final.IncidentsCaught, rep.Final.IncidentsTotal, rep.Final.IncidentRecall*100,
		rep.Final.SeverityCalibration*100,
		rep.Final.ControlsFalsePositive, rep.Final.ControlsTotal, rep.Final.FalsePositiveRate*100)
	for _, cs := range rep.Cases {
		fmt.Printf("  - %-42s (%s/%s) raw=%t verify=%t final=%t important=%t findings=%d\n",
			cs.CaseID, cs.Subset, cs.Kind, cs.RawHit, cs.PostVerifyHit, cs.FinalHit, cs.FinalImportant, cs.FinalFindings)
	}

	writeReport(rep, *outPath)
}

func writeReport(rep Report, outPath string) {
	if outPath == "" {
		return
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: marshal report: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outPath, append(b, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "revieweval: write %s: %v\n", outPath, err)
		os.Exit(1)
	}
}
