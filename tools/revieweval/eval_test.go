package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEmbeddedDatasetValidAndMatchesReviewMD(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "REVIEW.md"))
	if err != nil {
		t.Fatalf("read REVIEW.md: %v", err)
	}
	rules := ParseReviewRules(string(raw))
	if len(rules) < 10 {
		t.Fatalf("parsed only %d rules from REVIEW.md", len(rules))
	}
	cases, err := LoadCases("")
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if err := ValidateDataset(cases, rules); err != nil {
		t.Fatalf("ValidateDataset: %v", err)
	}

	// Whenever full git history is present, verify every case's commit exists,
	// its budgeted diff includes implementation source hunks ahead of docs, and
	// every incident case triggers its declared REVIEW.md rules.
	shallow := IsShallowClone(root)
	for _, c := range cases {
		paths, diff, ok, err := GitDiffForCase(root, c, 18000)
		if err != nil {
			t.Fatalf("GitDiffForCase(%s): %v", c.ID, err)
		}
		if !ok {
			if !shallow {
				t.Fatalf("case %s references commit %s which is missing in a full git clone", c.ID, c.Commit)
			}
			continue
		}
		if c.Kind == "incident" && !strings.Contains(diff, "diff --git a/internal/") {
			t.Errorf("incident case %s budgeted diff omitted internal/ implementation hunks", c.ID)
		}
		matched := MatchRules(rules, paths)
		got := map[string]bool{}
		for _, r := range matched {
			got[r.ID] = true
		}
		for _, want := range c.ExpectedRules {
			if !got[want] {
				t.Errorf("case %s expected REVIEW.md rule %s to trigger on %v", c.ID, want, paths)
			}
		}
	}
}

func TestScoreRunTracksStageDropsAndFalsePositives(t *testing.T) {
	cases := []Case{
		{
			ID:               "inc-1",
			Commit:           "1111111",
			Subset:           "tuning",
			Kind:             "incident",
			DiffDirection:    "reverse",
			Title:            "lock clock bug",
			ExpectedRules:    []string{"R02"},
			ExpectedSeverity: "Important",
			AnswerKey:        "uses now() after lock instead of clock_timestamp()",
			MatchGroups:      [][]string{{"clock_timestamp", "now()"}, {"lock", "redacted_at"}},
		},
		{
			ID:               "inc-2",
			Commit:           "2222222",
			Subset:           "eval",
			Kind:             "incident",
			DiffDirection:    "reverse",
			Title:            "cursor binding bug",
			ExpectedRules:    []string{"R05"},
			ExpectedSeverity: "Important",
			AnswerKey:        "cursor not bound to issuing list",
			MatchGroups:      [][]string{{"cursor", "page token"}, {"list", "binding"}},
		},
		{
			ID:            "ctrl-1",
			Commit:        "3333333",
			Subset:        "tuning",
			Kind:          "control",
			DiffDirection: "forward",
			Title:         "clean refactor",
		},
		{
			ID:            "ctrl-2",
			Commit:        "4444444",
			Subset:        "eval",
			Kind:          "control",
			DiffDirection: "forward",
			Title:         "clean docs",
		},
	}

	outputs := map[string]StageOutput{
		"inc-1": {
			Raw:        []Finding{{Severity: "Important", RuleID: "R02", Summary: "now() stamped after lock instead of clock_timestamp()"}},
			PostVerify: []Finding{{Severity: "Important", RuleID: "R02", Summary: "now() stamped after lock instead of clock_timestamp()"}},
			Final:      []Finding{{Severity: "Important", RuleID: "R02", Summary: "now() stamped after lock instead of clock_timestamp()"}},
		},
		// Simulate verifier over-filtering: caught at raw, dropped at post_verify.
		"inc-2": {
			Raw:        []Finding{{Severity: "Nit", RuleID: "R05", Summary: "cursor lacks list binding"}},
			PostVerify: nil,
			Final:      nil,
		},
		// Simulate verifier removing a false positive on ctrl-1.
		"ctrl-1": {
			Raw:        []Finding{{Severity: "Important", Summary: "speculative false alarm"}},
			PostVerify: []Finding{{Severity: "Nit", Summary: "minor naming nit"}},
			Final:      []Finding{{Severity: "Nit", Summary: "minor naming nit"}},
		},
		// ctrl-2 keeps an Important false positive at every stage.
		"ctrl-2": {
			Raw:        []Finding{{Severity: "Important", Summary: "wrongly flagged clean code"}},
			PostVerify: []Finding{{Severity: "Important", Summary: "wrongly flagged clean code"}},
			Final:      []Finding{{Severity: "Important", Summary: "wrongly flagged clean code"}},
		},
	}

	rep := ScoreRun(cases, outputs, nil)
	if rep.Raw.IncidentsCaught != 2 || rep.Raw.IncidentRecall != 1.0 {
		t.Errorf("raw recall = %d/%d (%.2f), want 2/2 (1.0)", rep.Raw.IncidentsCaught, rep.Raw.IncidentsTotal, rep.Raw.IncidentRecall)
	}
	if rep.Raw.SeverityCalibration != 0.5 {
		t.Errorf("raw severity calibration = %.2f, want 0.50 (1 Important + 1 Nit)", rep.Raw.SeverityCalibration)
	}
	if rep.Raw.FalsePositiveRate != 1.0 {
		t.Errorf("raw FP rate = %.2f, want 1.00", rep.Raw.FalsePositiveRate)
	}
	if rep.PostVerify.IncidentsCaught != 1 || rep.PostVerify.IncidentRecall != 0.5 {
		t.Errorf("post_verify recall = %d/%d, want 1/2", rep.PostVerify.IncidentsCaught, rep.PostVerify.IncidentsTotal)
	}
	if rep.Final.FalsePositiveRate != 0.5 {
		t.Errorf("final FP rate = %.2f, want 0.50 (verifier dropped ctrl-1 FP)", rep.Final.FalsePositiveRate)
	}
	if rep.Final.SeverityCalibration != 1.0 {
		t.Errorf("final severity calibration = %.2f, want 1.00", rep.Final.SeverityCalibration)
	}
}

func TestMatchesCaseRejectsGenericOverlappingVocabulary(t *testing.T) {
	cases, err := LoadCases("")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}

	// Unrelated Nit mentioning single quotes without the length/limit failure facet.
	if MatchesCase(byID["incident-1057-docker-single-quote-path"], Finding{
		Severity: "Nit",
		Summary:  "Add a comment documenting single quote handling",
	}) {
		t.Error("unrelated single-quote comment must not be credited as catching incident-1057")
	}

	// Unrelated R02 finding mentioning lock without now()/clock_timestamp().
	if MatchesCase(byID["incident-1037-redacted-at-lock-clock"], Finding{
		Severity: "Nit",
		RuleID:   "R02",
		Summary:  "Rename the lock helper for clarity",
	}) {
		t.Error("unrelated lock rename with R02 must not be credited as catching incident-1037")
	}

	// Nit-expected incident calibrates cleanly when matched by a Nit finding.
	nitCase := Case{
		ID:               "nit-inc",
		Kind:             "incident",
		Subset:           "eval",
		ExpectedSeverity: "Nit",
		MatchGroups:      [][]string{{"metric"}, {"label"}},
	}
	rep := ScoreRun([]Case{nitCase}, map[string]StageOutput{
		"nit-inc": {Final: []Finding{{Severity: "Nit", Summary: "metric missing label"}}},
	}, nil)
	if rep.Final.SeverityCalibration != 1.0 {
		t.Errorf("expected Nit severity calibration = 1.0, got %.2f", rep.Final.SeverityCalibration)
	}
}

func TestParseFindingsJSONRejectsNullAndInvalidSeverity(t *testing.T) {
	for _, bad := range []string{
		`null`,
		`{"findings": null}`,
		`[{"severity":"Critical","summary":"bad severity"}]`,
	} {
		if _, err := ParseFindingsJSON(bad); err == nil {
			t.Errorf("ParseFindingsJSON(%q) succeeded, want error", bad)
		}
	}
	got, err := ParseFindingsJSON(`[]`)
	if err != nil || len(got) != 0 {
		t.Fatalf("ParseFindingsJSON([]) = %v, %v, want empty non-error", got, err)
	}
}

func TestEndpointConfigDotEnvAndRedaction(t *testing.T) {
	tmp := t.TempDir()
	const secret = "sk-super-secret-value-never-print"
	dotenv := "MODEL_BASE_URL=[https://api.example.com/anthropic](https://api.example.com/anthropic)\nMODEL_API_KEY=" + secret + "\nMODEL_ID=MiniMax-M3\n"
	if err := os.WriteFile(filepath.Join(tmp, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadEndpointConfig(tmp, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadEndpointConfig: %v", err)
	}
	if cfg.Protocol != "anthropic" || cfg.BaseURL != "https://api.example.com/anthropic" || cfg.Model != "MiniMax-M3" {
		t.Fatalf("unexpected resolved config: %s", cfg)
	}
	for _, formatted := range []string{cfg.String(), fmt.Sprintf("%v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(formatted, secret) {
			t.Errorf("EndpointConfig leaked secret in formatted output: %s", formatted)
		}
	}

	// Upstream error responses that echo the API key must be redacted too.
	for _, proto := range []string{"openai", "anthropic"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "invalid key: "+secret, http.StatusUnauthorized)
		}))
		client := &LLMClient{Cfg: EndpointConfig{Protocol: proto, BaseURL: srv.URL, APIKey: secret, Model: "m"}}
		_, err := client.Complete(context.Background(), "sys", "usr")
		srv.Close()
		if err == nil {
			t.Fatalf("%s: expected error on 401", proto)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s error message leaked secret: %v", proto, err)
		}
	}
}

func TestRunTwoStageReviewOpenAIAndAnthropicMockServers(t *testing.T) {
	for _, proto := range []string{"openai", "anthropic"} {
		t.Run(proto, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				var text string
				if n == 1 {
					text = "```json\n[{\"severity\":\"Important\",\"rule_id\":\"R02\",\"summary\":\"now() stamped after lock instead of clock_timestamp()\",\"evidence\":\"SET redacted_at = now()\"},{\"severity\":\"Important\",\"rule_id\":\"R02\",\"summary\":\"now() stamped after lock instead of clock_timestamp()\",\"evidence\":\"dup\"},{\"severity\":\"Nit\",\"summary\":\"dropped by verifier\"}]\n```"
				} else {
					// Return duplicate verified findings so final-stage deduplication is exercised.
					text = `[{"severity":"Important","rule_id":"R02","summary":"now() stamped after lock instead of clock_timestamp()","evidence":"SET redacted_at = now()"},{"severity":"Important","rule_id":"R02","summary":"now() stamped after lock instead of clock_timestamp()","evidence":"dup"}]`
				}
				w.Header().Set("Content-Type", "application/json")
				if proto == "anthropic" {
					if r.Header.Get("x-api-key") != "test-key" {
						http.Error(w, "missing x-api-key", http.StatusUnauthorized)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"content": []map[string]string{{"type": "text", "text": text}},
					})
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-key" {
					http.Error(w, "missing bearer", http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]string{"content": text}}},
				})
			}))
			t.Cleanup(srv.Close)

			client := &LLMClient{Cfg: EndpointConfig{
				Protocol: proto,
				BaseURL:  srv.URL,
				APIKey:   "test-key",
				Model:    "mock-model",
			}}
			out, err := RunTwoStageReview(context.Background(), client, []ReviewRule{{ID: "R02", Title: "Lock vs clock", Check: "Use clock_timestamp()"}}, "- clock_timestamp()\n+ now()")
			if err != nil {
				t.Fatalf("RunTwoStageReview(%s): %v", proto, err)
			}
			if len(out.Raw) != 3 || len(out.PostVerify) != 2 || len(out.Final) != 1 {
				t.Fatalf("stage counts raw=%d verify=%d final=%d, want 3/2/1", len(out.Raw), len(out.PostVerify), len(out.Final))
			}
		})
	}
}
