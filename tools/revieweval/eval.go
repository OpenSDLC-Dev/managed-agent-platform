// Package main implements tools/revieweval: a meta-evaluation harness for
// measuring code-review quality over curated historical incident/revert PRs
// and clean control PRs (plan 75 slice 8).
//
// It tracks:
//   - stage-by-stage incident recall (raw -> post_verify -> final),
//   - control false-positive rate (Important findings raised on clean PRs),
//   - severity calibration (Important vs Nit/Pre-existing on true catches),
//   - REVIEW.md diff-trigger coverage on historical incidents.
//
// Offline dry-run mode (-dry-run) runs with zero network calls or credentials.
// Live mode (-live) calls any OpenAI-compatible or Anthropic-compatible LLM
// endpoint configured via REVIEWEVAL_* or the repository's .env MODEL_* keys.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed cases.json
var embeddedCasesJSON []byte

// Case is one curated review evaluation case backed by a real commit in this
// repository's git history.
type Case struct {
	ID               string   `json:"id"`
	PR               int      `json:"pr"`
	Commit           string   `json:"commit"`
	Subset           string   `json:"subset"`         // "tuning" | "eval"
	Kind             string   `json:"kind"`           // "incident" | "control"
	DiffDirection    string   `json:"diff_direction"` // "forward" | "reverse"
	Title            string   `json:"title"`
	ExpectedRules    []string `json:"expected_rules"`
	ExpectedSeverity string   `json:"expected_severity"`
	AnswerKey        string   `json:"answer_key"`
	// MatchGroups encodes AND-of-ORs defect facets: a finding matches the case
	// only when it satisfies EVERY facet group (preventing unrelated findings
	// sharing one generic subsystem noun from counting as incident recall).
	MatchGroups [][]string `json:"match_groups"`
}

// ReviewRule represents one parsed diff-triggered checklist item from REVIEW.md.
type ReviewRule struct {
	ID       string
	Title    string
	Triggers []string
	Check    string
}

// Finding is a single structured review finding returned by a reviewer stage.
type Finding struct {
	Severity string `json:"severity"` // "Important" | "Nit" | "Pre-existing"
	RuleID   string `json:"rule_id,omitempty"`
	Summary  string `json:"summary"`
	Evidence string `json:"evidence,omitempty"`
}

// StageOutput holds the findings emitted at each stage of a multi-stage
// review pipeline so drops between raw -> post_verify -> final are observable.
type StageOutput struct {
	Raw        []Finding `json:"raw"`
	PostVerify []Finding `json:"post_verify"`
	Final      []Finding `json:"final"`
}

// StageMetrics summarizes recall, false-positive rate, and severity
// calibration for a single pipeline stage.
type StageMetrics struct {
	IncidentsTotal        int     `json:"incidents_total"`
	IncidentsCaught       int     `json:"incidents_caught"`
	IncidentRecall        float64 `json:"incident_recall"`
	IncidentsImportant    int     `json:"incidents_important"`
	SeverityCalibration   float64 `json:"severity_calibration"`
	ControlsTotal         int     `json:"controls_total"`
	ControlsFalsePositive int     `json:"controls_false_positive"`
	FalsePositiveRate     float64 `json:"false_positive_rate"`
}

// CaseScore records the per-case outcome across all stages.
type CaseScore struct {
	CaseID         string   `json:"case_id"`
	Subset         string   `json:"subset"`
	Kind           string   `json:"kind"`
	TriggeredRules []string `json:"triggered_rules,omitempty"`
	RawHit         bool     `json:"raw_hit"`
	PostVerifyHit  bool     `json:"post_verify_hit"`
	FinalHit       bool     `json:"final_hit"`
	FinalImportant bool     `json:"final_important"`
	FinalFindings  int      `json:"final_findings"`
}

// Report is the aggregated evaluation result across all evaluated cases.
type Report struct {
	Mode       string                  `json:"mode"`
	Subset     string                  `json:"subset"`
	Model      string                  `json:"model,omitempty"`
	Raw        StageMetrics            `json:"raw"`
	PostVerify StageMetrics            `json:"post_verify"`
	Final      StageMetrics            `json:"final"`
	BySubset   map[string]StageMetrics `json:"by_subset,omitempty"`
	Cases      []CaseScore             `json:"cases"`
}

// EndpointConfig holds the LLM API configuration for live review runs.
// String/Format redact APIKey so credentials cannot leak into logs.
type EndpointConfig struct {
	Protocol string // "openai" | "anthropic"
	BaseURL  string
	APIKey   string
	Model    string
}

func (c EndpointConfig) String() string {
	key := "unset"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return fmt.Sprintf("{Protocol:%s BaseURL:%s APIKey:%s Model:%s}", c.Protocol, c.BaseURL, key, c.Model)
}

func (c EndpointConfig) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = io.WriteString(f, "EndpointConfig"+c.String())
		return
	}
	_, _ = io.WriteString(f, c.String())
}

// LoadCases loads the dataset from path (or the embedded cases.json when empty).
func LoadCases(filePath string) ([]Case, error) {
	raw := embeddedCasesJSON
	if filePath != "" {
		var err error
		raw, err = os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read cases %s: %w", filePath, err)
		}
	}
	var cases []Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, fmt.Errorf("decode cases: %w", err)
	}
	return cases, nil
}

var (
	ruleHeadingRe = regexp.MustCompile(`^###\s+(R\d{2})\s+[—-]\s+(.+)$`)
	backtickRe    = regexp.MustCompile("`([^`]+)`")
)

// ParseReviewRules extracts diff-triggered checklist items from REVIEW.md.
func ParseReviewRules(md string) []ReviewRule {
	var rules []ReviewRule
	var cur *ReviewRule
	sc := bufio.NewScanner(strings.NewReader(md))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := ruleHeadingRe.FindStringSubmatch(line); m != nil {
			if cur != nil {
				rules = append(rules, *cur)
			}
			cur = &ReviewRule{ID: m[1], Title: strings.TrimSpace(m[2])}
			continue
		}
		if strings.HasPrefix(line, "## ") && cur != nil {
			rules = append(rules, *cur)
			cur = nil
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(line, "- **Trigger:**") {
			for _, bm := range backtickRe.FindAllStringSubmatch(line, -1) {
				cur.Triggers = append(cur.Triggers, bm[1])
			}
		} else if strings.HasPrefix(line, "- **Check:**") {
			cur.Check = strings.TrimSpace(strings.TrimPrefix(line, "- **Check:**"))
		}
	}
	if cur != nil {
		rules = append(rules, *cur)
	}
	return rules
}

// MatchRules returns all REVIEW.md rules triggered by any path in changedPaths.
func MatchRules(rules []ReviewRule, changedPaths []string) []ReviewRule {
	var matched []ReviewRule
	for _, r := range rules {
		if ruleMatchesAny(r.Triggers, changedPaths) {
			matched = append(matched, r)
		}
	}
	return matched
}

func ruleMatchesAny(triggers, paths []string) bool {
	for _, p := range paths {
		for _, trig := range triggers {
			if triggerMatchesPath(trig, p) {
				return true
			}
		}
	}
	return false
}

func triggerMatchesPath(trig, p string) bool {
	if strings.HasSuffix(trig, "/") {
		return strings.HasPrefix(p, trig)
	}
	if strings.Contains(trig, "*") {
		ok, _ := path.Match(trig, p)
		if ok {
			return true
		}
		okBase, _ := path.Match(trig, path.Base(p))
		return okBase
	}
	if p == trig {
		return true
	}
	return strings.HasPrefix(p, trig) || strings.HasSuffix(p, trig)
}

// ValidateDataset verifies structural invariants, subset balance, and
// cross-references against REVIEW.md.
func ValidateDataset(cases []Case, rules []ReviewRule) error {
	if len(cases) == 0 {
		return errors.New("dataset is empty")
	}
	knownRules := map[string]bool{}
	for _, r := range rules {
		knownRules[r.ID] = true
	}
	seenID := map[string]bool{}
	counts := map[string]map[string]int{
		"tuning": {"incident": 0, "control": 0},
		"eval":   {"incident": 0, "control": 0},
	}

	for i, c := range cases {
		if c.ID == "" {
			return fmt.Errorf("case[%d]: empty id", i)
		}
		if seenID[c.ID] {
			return fmt.Errorf("case %s: duplicate id", c.ID)
		}
		seenID[c.ID] = true
		if len(c.Commit) < 7 {
			return fmt.Errorf("case %s: commit %q too short", c.ID, c.Commit)
		}
		if c.Subset != "tuning" && c.Subset != "eval" {
			return fmt.Errorf("case %s: invalid subset %q", c.ID, c.Subset)
		}
		if c.Kind != "incident" && c.Kind != "control" {
			return fmt.Errorf("case %s: invalid kind %q", c.ID, c.Kind)
		}
		if c.DiffDirection != "forward" && c.DiffDirection != "reverse" {
			return fmt.Errorf("case %s: invalid diff_direction %q", c.ID, c.DiffDirection)
		}
		if c.Title == "" {
			return fmt.Errorf("case %s: empty title", c.ID)
		}
		if c.Kind == "incident" {
			if strings.TrimSpace(c.AnswerKey) == "" {
				return fmt.Errorf("case %s: incident missing answer_key", c.ID)
			}
			if len(c.MatchGroups) < 2 {
				return fmt.Errorf("case %s: incident must define at least 2 defect facet match_groups", c.ID)
			}
			for gi, g := range c.MatchGroups {
				if len(g) == 0 {
					return fmt.Errorf("case %s: match_groups[%d] is empty", c.ID, gi)
				}
			}
			if c.ExpectedSeverity != "Important" && c.ExpectedSeverity != "Nit" {
				return fmt.Errorf("case %s: invalid expected_severity %q", c.ID, c.ExpectedSeverity)
			}
		}
		for _, rid := range c.ExpectedRules {
			if len(knownRules) > 0 && !knownRules[rid] {
				return fmt.Errorf("case %s: unknown REVIEW.md rule %q", c.ID, rid)
			}
		}
		counts[c.Subset][c.Kind]++
	}

	for _, subset := range []string{"tuning", "eval"} {
		if counts[subset]["incident"] == 0 || counts[subset]["control"] == 0 {
			return fmt.Errorf("subset %q must include both incident and control cases (got %+v)", subset, counts[subset])
		}
	}
	return nil
}

// MatchesCase reports whether a finding identifies every required defect facet
// in c.MatchGroups. Requiring every facet group (AND-of-ORs) prevents generic
// comments mentioning one subsystem noun or rule ID without naming the actual
// defect from inflating incident recall.
func MatchesCase(c Case, f Finding) bool {
	if c.Kind != "incident" || len(c.MatchGroups) == 0 {
		return false
	}
	text := strings.ToLower(f.Summary + " " + f.Evidence)
	for _, group := range c.MatchGroups {
		matchedGroup := false
		for _, kw := range group {
			if strings.Contains(text, strings.ToLower(kw)) {
				matchedGroup = true
				break
			}
		}
		if !matchedGroup {
			return false
		}
	}
	return true
}

func isImportant(sev string) bool {
	return strings.EqualFold(strings.TrimSpace(sev), "Important")
}

func evalStage(c Case, findings []Finding) (hit bool, calibratedHit bool, controlFP bool) {
	if c.Kind == "incident" {
		wantSev := c.ExpectedSeverity
		if wantSev == "" {
			wantSev = "Important"
		}
		for _, f := range findings {
			if MatchesCase(c, f) {
				hit = true
				if strings.EqualFold(strings.TrimSpace(f.Severity), wantSev) {
					calibratedHit = true
				}
			}
		}
		return hit, calibratedHit, false
	}
	for _, f := range findings {
		if isImportant(f.Severity) {
			return false, false, true
		}
	}
	return false, false, false
}

func finalizeMetrics(m *StageMetrics) {
	if m.IncidentsTotal > 0 {
		m.IncidentRecall = float64(m.IncidentsCaught) / float64(m.IncidentsTotal)
	}
	if m.IncidentsCaught > 0 {
		m.SeverityCalibration = float64(m.IncidentsImportant) / float64(m.IncidentsCaught)
	}
	if m.ControlsTotal > 0 {
		m.FalsePositiveRate = float64(m.ControlsFalsePositive) / float64(m.ControlsTotal)
	}
}

// ScoreRun computes stage-by-stage recall, control false-positive rate, and
// severity calibration across all evaluated cases.
func ScoreRun(cases []Case, outputs map[string]StageOutput, triggered map[string][]string) Report {
	rep := Report{
		BySubset: map[string]StageMetrics{},
	}
	subAcc := map[string]*StageMetrics{}

	for _, c := range cases {
		out := outputs[c.ID]
		rHit, rImp, rFP := evalStage(c, out.Raw)
		pHit, pImp, pFP := evalStage(c, out.PostVerify)
		fHit, fImp, fFP := evalStage(c, out.Final)

		accStage := func(m *StageMetrics, hit, imp, fp bool) {
			if c.Kind == "incident" {
				m.IncidentsTotal++
				if hit {
					m.IncidentsCaught++
				}
				if imp {
					m.IncidentsImportant++
				}
			} else {
				m.ControlsTotal++
				if fp {
					m.ControlsFalsePositive++
				}
			}
		}

		accStage(&rep.Raw, rHit, rImp, rFP)
		accStage(&rep.PostVerify, pHit, pImp, pFP)
		accStage(&rep.Final, fHit, fImp, fFP)

		sm, ok := subAcc[c.Subset]
		if !ok {
			sm = &StageMetrics{}
			subAcc[c.Subset] = sm
		}
		accStage(sm, fHit, fImp, fFP)

		rep.Cases = append(rep.Cases, CaseScore{
			CaseID:         c.ID,
			Subset:         c.Subset,
			Kind:           c.Kind,
			TriggeredRules: triggered[c.ID],
			RawHit:         rHit,
			PostVerifyHit:  pHit,
			FinalHit:       fHit,
			FinalImportant: fImp,
			FinalFindings:  len(out.Final),
		})
	}

	finalizeMetrics(&rep.Raw)
	finalizeMetrics(&rep.PostVerify)
	finalizeMetrics(&rep.Final)
	for k, sm := range subAcc {
		finalizeMetrics(sm)
		rep.BySubset[k] = *sm
	}
	return rep
}

// ParseDotEnv reads simple KEY=VALUE pairs from a .env file without expanding
// shell commands.
func ParseDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

var markdownLinkRe = regexp.MustCompile(`^\[[^\]]+\]\((https?://[^)]+)\)$`)

func cleanBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := markdownLinkRe.FindStringSubmatch(raw); m != nil {
		raw = m[1]
	}
	return strings.TrimRight(raw, "/")
}

// LoadEndpointConfig resolves the LLM endpoint from REVIEWEVAL_* env vars,
// falling back to MODEL_* env vars and then repoRoot/.env.
func LoadEndpointConfig(repoRoot string, getenv func(string) string) (EndpointConfig, error) {
	dotenv := map[string]string{}
	if repoRoot != "" {
		if m, err := ParseDotEnv(filepath.Join(repoRoot, ".env")); err == nil {
			dotenv = m
		}
	}
	lookup := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(getenv(k)); v != "" {
				return v
			}
		}
		for _, k := range keys {
			if v := strings.TrimSpace(dotenv[k]); v != "" {
				return v
			}
		}
		return ""
	}

	cfg := EndpointConfig{
		Protocol: strings.ToLower(lookup("REVIEWEVAL_PROTOCOL", "MODEL_PROTOCOL")),
		BaseURL:  cleanBaseURL(lookup("REVIEWEVAL_BASE_URL", "MODEL_BASE_URL")),
		APIKey:   lookup("REVIEWEVAL_API_KEY", "MODEL_API_KEY"),
		Model:    lookup("REVIEWEVAL_MODEL", "MODEL_ID"),
	}
	if cfg.Protocol == "" {
		if strings.Contains(strings.ToLower(cfg.BaseURL), "/anthropic") {
			cfg.Protocol = "anthropic"
		} else {
			cfg.Protocol = "openai"
		}
	}
	if cfg.Protocol != "openai" && cfg.Protocol != "anthropic" {
		return EndpointConfig{}, fmt.Errorf("unsupported protocol %q (want openai or anthropic)", cfg.Protocol)
	}
	if cfg.BaseURL == "" {
		return EndpointConfig{}, errors.New("missing REVIEWEVAL_BASE_URL / MODEL_BASE_URL")
	}
	if cfg.APIKey == "" {
		return EndpointConfig{}, errors.New("missing REVIEWEVAL_API_KEY / MODEL_API_KEY")
	}
	if cfg.Model == "" {
		return EndpointConfig{}, errors.New("missing REVIEWEVAL_MODEL / MODEL_ID")
	}
	return cfg, nil
}

// IsShallowClone reports whether repoRoot is a shallow git clone.
func IsShallowClone(repoRoot string) bool {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) == "true"
}

// diffPriority orders changed paths so implementation source hunks always fit
// inside maxBytes before test files and documentation/changelog noise.
func diffPriority(p string) int {
	switch {
	case strings.HasPrefix(p, "changelog.d/") || strings.HasPrefix(p, "docs/") || strings.HasSuffix(p, ".md"):
		return 2
	case strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_test.py"):
		return 1
	default:
		return 0
	}
}

// GitDiffForCase returns the changed paths and unified diff (capped at maxBytes,
// prioritizing implementation source files ahead of tests and docs) for a
// historical case. When the commit is absent (e.g. a shallow CI clone), ok is
// false and err is nil.
func GitDiffForCase(repoRoot string, c Case, maxBytes int) (changedPaths []string, diff string, ok bool, err error) {
	check := exec.Command("git", "-C", repoRoot, "cat-file", "-e", c.Commit+"^{commit}")
	if err := check.Run(); err != nil {
		return nil, "", false, nil
	}
	parentCheck := exec.Command("git", "-C", repoRoot, "cat-file", "-e", c.Commit+"^^{commit}")
	if err := parentCheck.Run(); err != nil {
		return nil, "", false, nil
	}

	fromRef, toRef := c.Commit+"^", c.Commit
	if c.DiffDirection == "reverse" {
		fromRef, toRef = c.Commit, c.Commit+"^"
	}

	namesOut, err := exec.Command("git", "-C", repoRoot, "diff", "--no-renames", "--name-only", fromRef, toRef).Output()
	if err != nil {
		return nil, "", false, fmt.Errorf("git diff --name-only %s..%s: %w", fromRef, toRef, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(namesOut)), "\n") {
		if line != "" {
			changedPaths = append(changedPaths, line)
		}
	}
	sort.Strings(changedPaths)

	ordered := append([]string(nil), changedPaths...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return diffPriority(ordered[i]) < diffPriority(ordered[j])
	})

	var buf bytes.Buffer
	truncated := false
	for _, p := range ordered {
		part, err := exec.Command("git", "-C", repoRoot, "diff", "--no-renames", "-U3", fromRef, toRef, "--", p).Output()
		if err != nil {
			return nil, "", false, fmt.Errorf("git diff %s..%s -- %s: %w", fromRef, toRef, p, err)
		}
		if maxBytes > 0 && buf.Len()+len(part) > maxBytes {
			rem := maxBytes - buf.Len()
			if rem > 0 {
				buf.Write(part[:rem])
			}
			truncated = true
			break
		}
		buf.Write(part)
	}
	if truncated {
		buf.WriteString("\n... [diff truncated for revieweval budget] ...\n")
	}
	return changedPaths, buf.String(), true, nil
}

// LLMClient calls an OpenAI-compatible or Anthropic-compatible endpoint.
type LLMClient struct {
	Cfg  EndpointConfig
	HTTP *http.Client
}

// Complete sends system+user prompts and returns the assistant text response.
func (c *LLMClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	switch c.Cfg.Protocol {
	case "anthropic":
		return c.completeAnthropic(ctx, hc, systemPrompt, userPrompt)
	case "openai":
		return c.completeOpenAI(ctx, hc, systemPrompt, userPrompt)
	default:
		return "", fmt.Errorf("unsupported protocol %q", c.Cfg.Protocol)
	}
}

func (c *LLMClient) redact(s string) string {
	if c.Cfg.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Cfg.APIKey, "[redacted]")
}

func (c *LLMClient) completeAnthropic(ctx context.Context, hc *http.Client, systemPrompt, userPrompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      c.Cfg.Model,
		"max_tokens": 1500,
		"system":     systemPrompt,
		"messages":   []map[string]string{{"role": "user", "content": userPrompt}},
	})
	if err != nil {
		return "", err
	}
	url := c.Cfg.BaseURL + "/v1/messages"
	if strings.HasSuffix(c.Cfg.BaseURL, "/v1") {
		url = c.Cfg.BaseURL + "/messages"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.Cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic endpoint status %d: %s", resp.StatusCode, truncateString(c.redact(string(raw)), 300))
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode anthropic response: %w", err)
	}
	var sb strings.Builder
	for _, blk := range parsed.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}
	return sb.String(), nil
}

func (c *LLMClient) completeOpenAI(ctx context.Context, hc *http.Client, systemPrompt, userPrompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":       c.Cfg.Model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
	})
	if err != nil {
		return "", err
	}
	url := c.Cfg.BaseURL + "/chat/completions"
	if !strings.HasSuffix(c.Cfg.BaseURL, "/v1") && !strings.Contains(c.Cfg.BaseURL, "/v1/") {
		url = c.Cfg.BaseURL + "/v1/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Cfg.APIKey)

	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai endpoint status %d: %s", resp.StatusCode, truncateString(c.redact(string(raw)), 300))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode openai response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("openai response contained no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func normalizeSeverity(sev string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "important":
		return "Important", true
	case "nit":
		return "Nit", true
	case "pre-existing":
		return "Pre-existing", true
	default:
		return "", false
	}
}

func validateFindings(arr []Finding) ([]Finding, error) {
	if arr == nil {
		return nil, errors.New("findings JSON must be a non-null array")
	}
	out := make([]Finding, len(arr))
	for i, f := range arr {
		norm, ok := normalizeSeverity(f.Severity)
		if !ok {
			return nil, fmt.Errorf("finding[%d]: unsupported severity %q (want Important, Nit, or Pre-existing)", i, f.Severity)
		}
		f.Severity = norm
		out[i] = f
	}
	return out, nil
}

// ParseFindingsJSON extracts a non-null JSON array (or {"findings": [...]}) of
// Findings from an LLM response, rejecting null and unsupported severities.
func ParseFindingsJSON(raw string) ([]Finding, error) {
	s := strings.TrimSpace(raw)
	if idx := strings.Index(s, "```"); idx >= 0 {
		rest := s[idx+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			s = strings.TrimSpace(rest[:end])
		}
	}
	var arr []Finding
	if err := json.Unmarshal([]byte(s), &arr); err == nil {
		return validateFindings(arr)
	}
	var obj struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		if obj.Findings == nil {
			return nil, errors.New("findings object field is null or missing")
		}
		return validateFindings(obj.Findings)
	}
	start, end := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(s[start:end+1]), &arr); err == nil {
			return validateFindings(arr)
		}
	}
	return nil, fmt.Errorf("could not parse findings JSON from model output: %s", truncateString(raw, 240))
}

// RunTwoStageReview executes the raw reviewer pass followed by an adversarial
// verification pass that drops ungrounded or speculative claims.
func RunTwoStageReview(ctx context.Context, client *LLMClient, triggered []ReviewRule, diff string) (StageOutput, error) {
	var checklist strings.Builder
	for _, r := range triggered {
		fmt.Fprintf(&checklist, "- %s (%s): %s\n", r.ID, r.Title, r.Check)
	}
	if checklist.Len() == 0 {
		checklist.WriteString("- No specialized REVIEW.md rules triggered; apply general correctness and security judgment.\n")
	}

	rawSys := `You are a senior code reviewer evaluating a git diff for a Go platform repository.
Return ONLY a strict JSON array of findings (or [] if the diff looks clean).
Each finding MUST be an object with keys: "severity" ("Important", "Nit", or "Pre-existing"), "rule_id" (e.g. "R02" or ""), "summary" (1-2 concise sentences), and "evidence" (exact code/symbol cited from the diff).
Be conservative: mark a finding "Important" ONLY for genuine bugs, regressions, security flaws, concurrency races, wire-compatibility breaks, or data-integrity hazards evidenced in the diff.`

	rawUser := fmt.Sprintf("Triggered REVIEW.md checklist items:\n%s\nUnified diff:\n```diff\n%s\n```", checklist.String(), diff)
	rawResp, err := client.Complete(ctx, rawSys, rawUser)
	if err != nil {
		return StageOutput{}, fmt.Errorf("raw review pass: %w", err)
	}
	rawFindings, err := ParseFindingsJSON(rawResp)
	if err != nil {
		return StageOutput{}, fmt.Errorf("parse raw findings: %w", err)
	}

	out := StageOutput{Raw: rawFindings}
	if len(rawFindings) == 0 {
		out.PostVerify = []Finding{}
		out.Final = []Finding{}
		return out, nil
	}

	rawJSON, _ := json.MarshalIndent(rawFindings, "", "  ")
	verifySys := `You are an adversarial verification filter auditing candidate code-review findings against the actual diff.
Keep ONLY findings that are directly substantiated by the diff and represent real defects; drop speculative, duplicate, or nitpicky false positives.
Return ONLY a strict JSON array using the same schema: [{"severity":"Important|Nit|Pre-existing","rule_id":"...","summary":"...","evidence":"..."}].`
	verifyUser := fmt.Sprintf("Candidate findings:\n%s\n\nUnified diff:\n```diff\n%s\n```", rawJSON, diff)

	verifyResp, err := client.Complete(ctx, verifySys, verifyUser)
	if err != nil {
		return StageOutput{}, fmt.Errorf("verification pass: %w", err)
	}
	verified, err := ParseFindingsJSON(verifyResp)
	if err != nil {
		return StageOutput{}, fmt.Errorf("parse verified findings: %w", err)
	}
	out.PostVerify = verified

	// Final stage deduplicates identical (rule_id, summary) pairs and keeps
	// only non-empty summaries.
	seen := map[string]bool{}
	final := make([]Finding, 0, len(verified))
	for _, f := range verified {
		s := strings.TrimSpace(f.Summary)
		if s == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(f.RuleID) + "|" + s)
		if seen[key] {
			continue
		}
		seen[key] = true
		final = append(final, f)
	}
	out.Final = final
	return out, nil
}
