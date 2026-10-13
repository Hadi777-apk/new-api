package service

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/QuantumNous/new-api/common"
)

// Image responses carry multi-MB b64_json strings, and every encoding/json
// round trip validates and copies them again. Detaching swaps each
// data[i].b64_json string for a short placeholder token so metadata edits work
// on kilobytes; attaching restores the original bytes exactly.
//
// Equivalence: every detached region is a complete, valid JSON string token,
// and the placeholder is one too, so the stripped body is valid JSON exactly
// when the original is. Anything unusual (duplicate or escaped keys, deep
// nesting) skips detaching and callers run their original full round trip.
type imageBase64Blobs struct {
	tokens [][]byte
	raws   [][]byte
}

var errImageJSONScan = errors.New("image JSON scan stopped")

const imageJSONScanMaxDepth = 64

type imageJSONScanner struct {
	body  []byte
	pos   int
	spans [][2]int
}

// detachImageBase64 returns body unchanged and nil when there is nothing to
// detach or the body is not plain valid JSON, so callers keep their behavior.
func detachImageBase64(body []byte) ([]byte, *imageBase64Blobs) {
	scanner := imageJSONScanner{body: body}
	if scanner.scanTop() != nil || len(scanner.spans) == 0 {
		return body, nil
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return body, nil
	}
	prefix := `"__image_b64_` + hex.EncodeToString(nonce) + `_`
	blobs := &imageBase64Blobs{}
	out := make([]byte, 0, 4096)
	last := 0
	for i, span := range scanner.spans {
		token := []byte(prefix + strconv.Itoa(i) + `__"`)
		out = append(out, body[last:span[0]]...)
		out = append(out, token...)
		blobs.tokens = append(blobs.tokens, token)
		blobs.raws = append(blobs.raws, body[span[0]:span[1]])
		last = span[1]
	}
	return append(out, body[last:]...), blobs
}

// attach puts the original b64_json bytes back. It reports false if any
// placeholder is missing or out of order, so callers can fall back.
func (b *imageBase64Blobs) attach(body []byte) ([]byte, bool) {
	if b == nil {
		return body, true
	}
	size := len(body)
	for _, raw := range b.raws {
		size += len(raw)
	}
	out := make([]byte, 0, size)
	last := 0
	for i, token := range b.tokens {
		index := bytes.Index(body[last:], token)
		if index < 0 {
			return nil, false
		}
		out = append(out, body[last:last+index]...)
		out = append(out, b.raws[i]...)
		last += index + len(token)
	}
	return append(out, body[last:]...), true
}

// original returns the detached JSON string token that a placeholder stands for.
func (b *imageBase64Blobs) original(raw json.RawMessage) json.RawMessage {
	if b == nil {
		return raw
	}
	for i, token := range b.tokens {
		if bytes.Equal(raw, token) {
			return b.raws[i]
		}
	}
	return raw
}

// imageBase64Reader decodes a b64_json JSON value. It streams plain strings
// without copying them and falls back to a full unquote only for escapes.
func imageBase64Reader(raw json.RawMessage) (io.Reader, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') >= 0 {
		var encoded string
		if common.Unmarshal(raw, &encoded) != nil {
			return nil, false
		}
		inner = []byte(encoded)
	}
	if len(inner) == 0 {
		return nil, false
	}
	return base64.NewDecoder(base64.StdEncoding, bytes.NewReader(inner)), true
}

// jsonStringValue unquotes a JSON string token, copying plain strings directly.
func jsonStringValue(raw json.RawMessage) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner), true
	}
	var value string
	if common.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// ImageJSONWithoutBase64 returns a copy of an Images response with b64_json
// strings replaced by short tokens, for reading usage, errors, or counts.
// Invalid JSON is returned unchanged so decoding still reports the error.
func ImageJSONWithoutBase64(body []byte) []byte {
	stripped, _ := detachImageBase64(body)
	return stripped
}

// scanTop walks {"data":[{"b64_json":"..."}...], ...} and records the spans of
// the b64_json string tokens. It validates the whole document's grammar.
func (s *imageJSONScanner) scanTop() error {
	s.skipSpace()
	if s.pos >= len(s.body) || s.body[s.pos] != '{' {
		return errImageJSONScan
	}
	seenData := false
	err := s.object(func(key []byte) error {
		if !bytes.Equal(key, []byte("data")) {
			return s.value(1)
		}
		if seenData {
			return errImageJSONScan
		}
		seenData = true
		s.skipSpace()
		if s.pos >= len(s.body) || s.body[s.pos] != '[' {
			return s.value(1)
		}
		return s.array(func() error {
			s.skipSpace()
			if s.pos >= len(s.body) || s.body[s.pos] != '{' {
				return s.value(2)
			}
			seenImage := false
			return s.object(func(key []byte) error {
				if !bytes.Equal(key, []byte("b64_json")) {
					return s.value(3)
				}
				if seenImage {
					return errImageJSONScan
				}
				seenImage = true
				s.skipSpace()
				if s.pos >= len(s.body) || s.body[s.pos] != '"' {
					return s.value(3)
				}
				start := s.pos
				if err := s.str(); err != nil {
					return err
				}
				s.spans = append(s.spans, [2]int{start, s.pos})
				return nil
			})
		})
	})
	if err != nil {
		return err
	}
	s.skipSpace()
	if s.pos != len(s.body) {
		return errImageJSONScan
	}
	return nil
}

func (s *imageJSONScanner) skipSpace() {
	for s.pos < len(s.body) {
		switch s.body[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

// object expects s.pos at '{' and calls member with s.pos before each value.
// Keys containing escapes stop the scan, since they could alias "data".
func (s *imageJSONScanner) object(member func(key []byte) error) error {
	s.pos++
	s.skipSpace()
	if s.pos < len(s.body) && s.body[s.pos] == '}' {
		s.pos++
		return nil
	}
	for {
		s.skipSpace()
		if s.pos >= len(s.body) || s.body[s.pos] != '"' {
			return errImageJSONScan
		}
		start := s.pos
		if err := s.str(); err != nil {
			return err
		}
		key := s.body[start+1 : s.pos-1]
		if bytes.IndexByte(key, '\\') >= 0 {
			return errImageJSONScan
		}
		s.skipSpace()
		if s.pos >= len(s.body) || s.body[s.pos] != ':' {
			return errImageJSONScan
		}
		s.pos++
		if err := member(key); err != nil {
			return err
		}
		s.skipSpace()
		if s.pos >= len(s.body) {
			return errImageJSONScan
		}
		switch s.body[s.pos] {
		case ',':
			s.pos++
		case '}':
			s.pos++
			return nil
		default:
			return errImageJSONScan
		}
	}
}

// array expects s.pos at '[' and calls element with s.pos before each value.
func (s *imageJSONScanner) array(element func() error) error {
	s.pos++
	s.skipSpace()
	if s.pos < len(s.body) && s.body[s.pos] == ']' {
		s.pos++
		return nil
	}
	for {
		if err := element(); err != nil {
			return err
		}
		s.skipSpace()
		if s.pos >= len(s.body) {
			return errImageJSONScan
		}
		switch s.body[s.pos] {
		case ',':
			s.pos++
		case ']':
			s.pos++
			return nil
		default:
			return errImageJSONScan
		}
	}
}

func (s *imageJSONScanner) value(depth int) error {
	if depth > imageJSONScanMaxDepth {
		return errImageJSONScan
	}
	s.skipSpace()
	if s.pos >= len(s.body) {
		return errImageJSONScan
	}
	switch c := s.body[s.pos]; {
	case c == '{':
		return s.object(func([]byte) error { return s.value(depth + 1) })
	case c == '[':
		return s.array(func() error { return s.value(depth + 1) })
	case c == '"':
		return s.str()
	case c == 't':
		return s.literal("true")
	case c == 'f':
		return s.literal("false")
	case c == 'n':
		return s.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return s.number()
	}
	return errImageJSONScan
}

func (s *imageJSONScanner) literal(word string) error {
	if !bytes.HasPrefix(s.body[s.pos:], []byte(word)) {
		return errImageJSONScan
	}
	s.pos += len(word)
	return nil
}

func (s *imageJSONScanner) number() error {
	b, i := s.body, s.pos
	digits := func() bool {
		start := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		return i > start
	}
	if b[i] == '-' {
		i++
	}
	if i < len(b) && b[i] == '0' {
		i++
	} else if !digits() {
		return errImageJSONScan
	}
	if i < len(b) && b[i] == '.' {
		i++
		if !digits() {
			return errImageJSONScan
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if !digits() {
			return errImageJSONScan
		}
	}
	s.pos = i
	return nil
}

// str expects s.pos at '"' and leaves it after the closing quote. Plain runs
// are found with IndexByte; every byte is still checked for control characters.
func (s *imageJSONScanner) str() error {
	b := s.body
	i := s.pos + 1
	for {
		quote := bytes.IndexByte(b[i:], '"')
		if quote < 0 {
			return errImageJSONScan
		}
		run := b[i : i+quote]
		escape := bytes.IndexByte(run, '\\')
		if escape >= 0 {
			run = run[:escape]
		}
		for _, c := range run {
			if c < 0x20 {
				return errImageJSONScan
			}
		}
		if escape < 0 {
			s.pos = i + quote + 1
			return nil
		}
		i += escape + 1
		if i >= len(b) {
			return errImageJSONScan
		}
		switch b[i] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i++
		case 'u':
			if i+4 >= len(b) {
				return errImageJSONScan
			}
			for _, h := range b[i+1 : i+5] {
				if !(h >= '0' && h <= '9' || h >= 'a' && h <= 'f' || h >= 'A' && h <= 'F') {
					return errImageJSONScan
				}
			}
			i += 5
		default:
			return errImageJSONScan
		}
	}
}
