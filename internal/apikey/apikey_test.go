package apikey_test

import (
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"testing"
)

// Hash is the hex SHA-256 the api_keys rows are keyed by, so a row written by
// one server authenticates on the other.
func TestHash(t *testing.T) {
	const want = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b" // sha256("secret")
	if got := apikey.Hash("secret"); got != want {
		t.Errorf("Hash = %s, want %s", got, want)
	}
}
