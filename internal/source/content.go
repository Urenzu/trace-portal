package source

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// DefaultMaxContent bounds one captured block.
//
// Sized from what agents actually produce: a prompt or an assistant message is
// a few kilobytes, and the long tail is entirely tool output -- a file read, a
// test run, a directory listing. 128 KiB keeps every message and every ordinary
// command whole while refusing to make the archive track the largest file
// anybody has ever opened. What is cut is recorded as cut; a truncated tool
// output read as complete would be a confident wrong answer about what the
// agent saw.
const DefaultMaxContent = 128 << 10

// capture builds the blob payload for one content record, truncating to limit.
func capture(c trace.Content, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxContent
	}
	if n := len(c.Text); n > limit {
		c.Bytes, c.Truncated = n, true
		c.Text = truncateUTF8(c.Text, limit)
	} else if c.Bytes == 0 {
		c.Bytes = n
	}
	if len(c.Input) > limit {
		// The arguments are kept as raw JSON so an unfamiliar tool schema
		// survives intact, and a truncated fragment of JSON is not JSON. Move
		// it to the text field, where a reader can still see what was sent
		// without anything downstream trying to parse it.
		c.Bytes, c.Truncated = len(c.Input), true
		c.Text = truncateUTF8(string(c.Input), limit)
		c.Input = nil
	}
	return json.Marshal(c)
}

// truncateUTF8 cuts at a rune boundary, so the result is still valid UTF-8 and
// still valid JSON once encoded.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
