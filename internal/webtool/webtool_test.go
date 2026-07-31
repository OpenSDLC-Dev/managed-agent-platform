package webtool

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
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

	t.Run("the cut never splits a rune", func(t *testing.T) {
		// "世" (3 bytes) straddles the cap: its first byte is the cap's last.
		src := strings.Repeat("a", MaxContentBytes-1) + "世"
		data, truncated, err := ReadCapped(strings.NewReader(src))
		if err != nil {
			t.Fatalf("ReadCapped: %v", err)
		}
		if !truncated {
			t.Error("a straddling body did not report truncated")
		}
		if len(data) != MaxContentBytes-1 {
			t.Errorf("len(data) = %d, want %d (the split rune dropped)", len(data), MaxContentBytes-1)
		}
		if !utf8.Valid(data) {
			t.Error("the cut left invalid UTF-8")
		}
	})

	t.Run("a binary body keeps the raw cut", func(t *testing.T) {
		src := bytes.Repeat([]byte{0x80}, MaxContentBytes+1)
		data, truncated, err := ReadCapped(bytes.NewReader(src))
		if err != nil {
			t.Fatalf("ReadCapped: %v", err)
		}
		if !truncated || len(data) != MaxContentBytes {
			t.Errorf("truncated=%v len=%d, want true/%d: no rune boundary to back to", truncated, len(data), MaxContentBytes)
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

	t.Run("redacts the status line too", func(t *testing.T) {
		// The reason phrase is server-supplied text: "500 <key>" must not
		// carry the key past redaction.
		err := HTTPError("op", "500 leaked k-123", []byte("body"), "k-123")
		if strings.Contains(err.Error(), "k-123") {
			t.Errorf("credential leaked via the status line: %v", err)
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

	t.Run("the excerpt cut never splits a rune", func(t *testing.T) {
		err := HTTPError("op", "500", []byte(strings.Repeat("x", maxSnippet-1)+"世"), "")
		if !utf8.ValidString(err.Error()) {
			t.Errorf("excerpt cut left invalid UTF-8: %q", err)
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
