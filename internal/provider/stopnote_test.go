package provider_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// StopNote is what an incomplete tool call's error says about how its stream
// ended; each of the three things it can know shows (#951).
func TestStopNote(t *testing.T) {
	for _, tc := range []struct {
		reason string
		usage  *domain.ModelUsage
		want   string
	}{
		{"max_tokens", &domain.ModelUsage{OutputTokens: 8192}, "stop_reason max_tokens, 8192 output tokens"},
		{"max_tokens", nil, "stop_reason max_tokens"},
		{"", &domain.ModelUsage{OutputTokens: 8192}, "no stop reason"},
	} {
		if got := provider.StopNote(tc.reason, tc.usage); got != tc.want {
			t.Errorf("StopNote(%q, %v) = %q, want %q", tc.reason, tc.usage, got, tc.want)
		}
	}
}
