package webtool

import (
	"strings"
	"testing"
)

func TestReadCapped(t *testing.T) {
	t.Run("a body at the cap is whole", func(t *testing.T) {
		src := strings.Repeat("a", MaxContentBytes)
		data, truncated, err := ReadCapped(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ReadCapped: %v", err)
		}
		if truncated {
			t.Error("a body exactly at the cap reported truncated")
		}
		if len(data) != MaxContentBytes {
			t.Errorf("len(data) = %d, want %d", len(data), MaxContentBytes)
		}
	})

	t.Run("a body past the cap truncates and says so", func(t *testing.T) {
		src := strings.Repeat("a", MaxContentBytes+1)
		data, truncated, err := ReadCapped(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ReadCapped: %v", err)
		}
		if !truncated {
			t.Error("a body past the cap did not report truncated")
		}
		if len(data) != MaxContentBytes {
			t.Errorf("len(data) = %d, want %d", len(data), MaxContentBytes)
		}
	})

	t.Run("a reader failure surfaces", func(t *testing.T) {
		if _, _, err := ReadCapped(failingReader{}); err == nil {
			t.Error("a failing reader did not surface as an error")
		}
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errRead }

var errRead = &readError{}

type readError struct{}

func (*readError) Error() string { return "read failed" }

func TestHTTPError(t *testing.T) {
	t.Run("redacts every occurrence of the credential", func(t *testing.T) {
		err := HTTPError("tavily search", "500 Internal Server Error",
			[]byte("bad key k-123, seriously k-123"), "k-123")
		if strings.Contains(err.Error(), "k-123") {
			t.Errorf("credential leaked: %v", err)
		}
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("no redaction marker: %v", err)
		}
	})

	t.Run("redacts before the excerpt is cut", func(t *testing.T) {
		// The credential straddles the excerpt boundary: cutting first would
		// keep its head.
		secret := strings.Repeat("s", 64)
		body := strings.Repeat("x", maxSnippet-10) + secret
		err := HTTPError("op", "500", []byte(body), secret)
		if strings.Contains(err.Error(), "sssss") {
			t.Errorf("credential fragment leaked: %v", err)
		}
	})

	t.Run("caps the excerpt and keeps op and status", func(t *testing.T) {
		err := HTTPError("jina fetch", "502 Bad Gateway",
			[]byte(strings.Repeat("x", 10*maxSnippet)), "")
		if got := len(err.Error()); got > maxSnippet+64 {
			t.Errorf("error length = %d, excerpt not capped", got)
		}
		for _, want := range []string{"jina fetch", "502 Bad Gateway"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})
}
