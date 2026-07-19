package main

import (
	"bytes"
	"strconv"
	"strings"
)

// requestMayContainMedia validates text-only JSON in one pass without
// materializing multi-megabyte message strings. Once a media discriminator is
// found, the regular decoder takes over and performs full validation.
func requestMayContainMedia(raw []byte) (bool, bool) {
	scanner := mediaJSONScanner{raw: raw}
	media, valid := scanner.scanValue(0)
	if media {
		return true, true
	}
	scanner.skipSpace()
	return false, valid && scanner.pos == len(raw)
}

type mediaJSONScanner struct {
	raw []byte
	pos int
}

func (s *mediaJSONScanner) scanValue(depth int) (bool, bool) {
	if depth > 10000 {
		return false, false
	}
	s.skipSpace()
	if s.pos >= len(s.raw) {
		return false, false
	}
	switch s.raw[s.pos] {
	case '{':
		return s.scanObject(depth + 1)
	case '[':
		return s.scanArray(depth + 1)
	case '"':
		return false, s.skipString()
	case 't':
		return false, s.skipLiteral("true")
	case 'f':
		return false, s.skipLiteral("false")
	case 'n':
		return false, s.skipLiteral("null")
	default:
		return false, s.skipNumber()
	}
}

func (s *mediaJSONScanner) scanObject(depth int) (bool, bool) {
	s.pos++
	s.skipSpace()
	if s.consume('}') {
		return false, true
	}
	for s.pos < len(s.raw) {
		key, valid := s.readKey()
		if !valid {
			return false, false
		}
		s.skipSpace()
		if !s.consume(':') {
			return false, false
		}
		s.skipSpace()
		if (key == "type" || key == "media_type") && s.pos < len(s.raw) && s.raw[s.pos] == '"' {
			value, valid := s.readShortString(256)
			if !valid {
				return false, false
			}
			if mediaDiscriminator(key, strings.ToLower(strings.TrimSpace(value))) {
				return true, true
			}
		} else {
			media, valid := s.scanValue(depth)
			if media || !valid {
				return media, valid
			}
		}
		s.skipSpace()
		if s.consume('}') {
			return false, true
		}
		if !s.consume(',') {
			return false, false
		}
		s.skipSpace()
	}
	return false, false
}

func (s *mediaJSONScanner) scanArray(depth int) (bool, bool) {
	s.pos++
	s.skipSpace()
	if s.consume(']') {
		return false, true
	}
	for s.pos < len(s.raw) {
		media, valid := s.scanValue(depth)
		if media || !valid {
			return media, valid
		}
		s.skipSpace()
		if s.consume(']') {
			return false, true
		}
		if !s.consume(',') {
			return false, false
		}
		s.skipSpace()
	}
	return false, false
}

func (s *mediaJSONScanner) readKey() (string, bool) {
	start := s.pos
	if !s.skipString() {
		return "", false
	}
	raw := s.raw[start+1 : s.pos-1]
	if !bytes.ContainsRune(raw, '\\') {
		switch string(raw) {
		case "type":
			return "type", true
		case "media_type":
			return "media_type", true
		default:
			return "", true
		}
	}
	if len(raw) > 64 {
		return "", true
	}
	decoded, err := strconv.Unquote(string(s.raw[start:s.pos]))
	if err != nil {
		return "", false
	}
	if decoded == "type" || decoded == "media_type" {
		return decoded, true
	}
	return "", true
}

func (s *mediaJSONScanner) readShortString(limit int) (string, bool) {
	start := s.pos
	if !s.skipString() {
		return "", false
	}
	if s.pos-start > limit {
		return "", true
	}
	decoded, err := strconv.Unquote(string(s.raw[start:s.pos]))
	return decoded, err == nil
}

func (s *mediaJSONScanner) skipString() bool {
	if s.pos >= len(s.raw) || s.raw[s.pos] != '"' {
		return false
	}
	s.pos++
	for s.pos < len(s.raw) {
		value := s.raw[s.pos]
		switch value {
		case '"':
			s.pos++
			return true
		case '\\':
			s.pos++
			if s.pos >= len(s.raw) {
				return false
			}
			escaped := s.raw[s.pos]
			if escaped == 'u' {
				if s.pos+4 >= len(s.raw) {
					return false
				}
				for offset := 1; offset <= 4; offset++ {
					if !isHex(s.raw[s.pos+offset]) {
						return false
					}
				}
				s.pos += 5
				continue
			}
			if !bytes.ContainsRune([]byte(`"\/bfnrt`), rune(escaped)) {
				return false
			}
			s.pos++
		default:
			if value < 0x20 {
				return false
			}
			s.pos++
		}
	}
	return false
}

func (s *mediaJSONScanner) skipLiteral(literal string) bool {
	if len(s.raw)-s.pos < len(literal) || string(s.raw[s.pos:s.pos+len(literal)]) != literal {
		return false
	}
	s.pos += len(literal)
	return true
}

func (s *mediaJSONScanner) skipNumber() bool {
	start := s.pos
	if s.consume('-') && s.pos >= len(s.raw) {
		return false
	}
	if s.consume('0') {
		if s.pos < len(s.raw) && s.raw[s.pos] >= '0' && s.raw[s.pos] <= '9' {
			return false
		}
	} else {
		if s.pos >= len(s.raw) || s.raw[s.pos] < '1' || s.raw[s.pos] > '9' {
			return false
		}
		for s.pos < len(s.raw) && s.raw[s.pos] >= '0' && s.raw[s.pos] <= '9' {
			s.pos++
		}
	}
	if s.consume('.') {
		fraction := s.pos
		for s.pos < len(s.raw) && s.raw[s.pos] >= '0' && s.raw[s.pos] <= '9' {
			s.pos++
		}
		if s.pos == fraction {
			return false
		}
	}
	if s.pos < len(s.raw) && (s.raw[s.pos] == 'e' || s.raw[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.raw) && (s.raw[s.pos] == '+' || s.raw[s.pos] == '-') {
			s.pos++
		}
		exponent := s.pos
		for s.pos < len(s.raw) && s.raw[s.pos] >= '0' && s.raw[s.pos] <= '9' {
			s.pos++
		}
		if s.pos == exponent {
			return false
		}
	}
	return s.pos > start
}

func (s *mediaJSONScanner) consume(value byte) bool {
	if s.pos >= len(s.raw) || s.raw[s.pos] != value {
		return false
	}
	s.pos++
	return true
}

func (s *mediaJSONScanner) skipSpace() {
	for s.pos < len(s.raw) {
		switch s.raw[s.pos] {
		case ' ', '\t', '\r', '\n':
			s.pos++
		default:
			return
		}
	}
}

func mediaDiscriminator(key, value string) bool {
	if key == "media_type" {
		return value == "application/pdf" || strings.HasPrefix(value, "image/") || strings.HasPrefix(value, "audio/") || strings.HasPrefix(value, "video/")
	}
	if isImageBlockType(value) || strings.Contains(value, "image") {
		return true
	}
	switch value {
	case "document", "pdf", "input_file", "file", "file_url", "audio", "input_audio", "video", "input_video", "screenshot", "computer_screenshot":
		return true
	default:
		return false
	}
}
