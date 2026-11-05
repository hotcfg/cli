package cli

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// pointerLines maps the JSON Pointer of every value in doc to the 1-based
// line it starts on, so schema errors (which name values by pointer) can
// point into the user's file. It returns what it found before any syntax
// error.
func pointerLines(doc []byte) map[string]int {
	out := map[string]int{}
	if len(doc) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	_ = walkJSON(dec, doc, "", out)
	return out
}

func walkJSON(dec *json.Decoder, doc []byte, ptr string, out map[string]int) error {
	// InputOffset is just past the previous token; the value starts after
	// any whitespace and the ':' or ',' separating it.
	start := skipSeparators(doc, int(dec.InputOffset()))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	out[ptr] = 1 + bytes.Count(doc[:start], []byte{'\n'})
	switch tok {
	case json.Delim('{'):
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			if err := walkJSON(dec, doc, ptr+"/"+escapePointer(key), out); err != nil {
				return err
			}
		}
		_, err = dec.Token() // '}'
	case json.Delim('['):
		for i := 0; dec.More(); i++ {
			if err := walkJSON(dec, doc, ptr+"/"+strconv.Itoa(i), out); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ']'
	}
	return err
}

func skipSeparators(doc []byte, i int) int {
	for i < len(doc) {
		switch doc[i] {
		case ' ', '\t', '\r', '\n', ':', ',':
			i++
		default:
			return i
		}
	}
	return i
}

// escapePointer escapes a key for a JSON Pointer (RFC 6901).
func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// lineCol converts a byte offset into a 1-based line and column.
func lineCol(doc []byte, offset int) (int, int) {
	offset = min(max(offset, 0), len(doc))
	before := doc[:offset]
	line := 1 + bytes.Count(before, []byte{'\n'})
	col := offset - bytes.LastIndexByte(before, '\n')
	return line, col
}
