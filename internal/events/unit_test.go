package events

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// White-box: chunkText must keep every chunk's JSON-escaped form within the
// budget, across the escape classes encoding/json actually emits.
func TestChunkTextEscapeBudget(t *testing.T) {
	// A pathological mix: quotes, backslashes, newlines, control chars,
	// HTML-escaped runes, U+2028/U+2029, and multibyte text.
	unit := "a\"b\\c\nd\x01e<f&g h 界"
	long := strings.Repeat(unit, 400)

	const budget = 100
	chunks := chunkText(long, budget)
	if strings.Join(chunks, "") != long {
		t.Fatal("chunks do not reassemble the input")
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		encoded, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		// len minus the surrounding quotes must fit the budget.
		if got := len(encoded) - 2; got > budget {
			t.Errorf("chunk %d escapes to %d bytes, budget %d", i, got, budget)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d split a rune", i)
		}
	}

	if got := chunkText("", 10); len(got) != 1 || got[0] != "" {
		t.Errorf("empty text chunks = %q", got)
	}
}
