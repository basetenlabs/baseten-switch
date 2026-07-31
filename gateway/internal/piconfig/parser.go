package piconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

type valueKind byte

const (
	kindObject valueKind = '{'
	kindArray  valueKind = '['
	kindScalar valueKind = 's'
)

type jsonValue struct {
	kind    valueKind
	start   int
	end     int
	open    int
	close   int
	members []jsonMember
	items   []jsonValue
}

type jsonMember struct {
	key      string
	keyStart int
	value    jsonValue
	comma    int
}

type jsoncParser struct {
	src []byte
	pos int
}

func parseJSONCDocument(src []byte) (jsonValue, error) {
	if !utf8.Valid(src) {
		return jsonValue{}, fmt.Errorf("invalid JSONC: input is not valid UTF-8")
	}
	p := jsoncParser{src: src}
	if err := p.skipTrivia(); err != nil {
		return jsonValue{}, err
	}
	if p.pos == len(src) {
		return jsonValue{}, p.errorf("expected a JSON value")
	}
	value, err := p.parseValue()
	if err != nil {
		return jsonValue{}, err
	}
	if err := p.skipTrivia(); err != nil {
		return jsonValue{}, err
	}
	if p.pos != len(src) {
		return jsonValue{}, p.errorf("unexpected content after the JSON value")
	}
	return value, nil
}

func (p *jsoncParser) parseValue() (jsonValue, error) {
	if err := p.skipTrivia(); err != nil {
		return jsonValue{}, err
	}
	if p.pos >= len(p.src) {
		return jsonValue{}, p.errorf("expected a JSON value")
	}
	switch p.src[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		start := p.pos
		if _, err := p.parseString(); err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: kindScalar, start: start, end: p.pos}, nil
	default:
		return p.parseScalar()
	}
}

func (p *jsoncParser) parseObject() (jsonValue, error) {
	start := p.pos
	p.pos++
	object := jsonValue{
		kind:  kindObject,
		start: start,
		open:  start,
	}
	if err := p.skipTrivia(); err != nil {
		return jsonValue{}, err
	}
	if p.consume('}') {
		object.close = p.pos - 1
		object.end = p.pos
		return object, nil
	}

	for {
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return jsonValue{}, p.errorf("expected an object key")
		}
		keyStart := p.pos
		key, err := p.parseString()
		if err != nil {
			return jsonValue{}, err
		}
		if err := p.skipTrivia(); err != nil {
			return jsonValue{}, err
		}
		if !p.consume(':') {
			return jsonValue{}, p.errorf("expected ':' after object key")
		}
		value, err := p.parseValue()
		if err != nil {
			return jsonValue{}, err
		}
		member := jsonMember{
			key:      key,
			keyStart: keyStart,
			value:    value,
			comma:    -1,
		}
		if err := p.skipTrivia(); err != nil {
			return jsonValue{}, err
		}
		if p.consume(',') {
			member.comma = p.pos - 1
			object.members = append(object.members, member)
			if err := p.skipTrivia(); err != nil {
				return jsonValue{}, err
			}
			if p.consume('}') {
				object.close = p.pos - 1
				object.end = p.pos
				return object, nil
			}
			continue
		}
		object.members = append(object.members, member)
		if !p.consume('}') {
			return jsonValue{}, p.errorf("expected ',' or '}' after object member")
		}
		object.close = p.pos - 1
		object.end = p.pos
		return object, nil
	}
}

func (p *jsoncParser) parseArray() (jsonValue, error) {
	start := p.pos
	p.pos++
	array := jsonValue{
		kind:  kindArray,
		start: start,
		open:  start,
	}
	if err := p.skipTrivia(); err != nil {
		return jsonValue{}, err
	}
	if p.consume(']') {
		array.close = p.pos - 1
		array.end = p.pos
		return array, nil
	}

	for {
		item, err := p.parseValue()
		if err != nil {
			return jsonValue{}, err
		}
		array.items = append(array.items, item)
		if err := p.skipTrivia(); err != nil {
			return jsonValue{}, err
		}
		if p.consume(',') {
			if err := p.skipTrivia(); err != nil {
				return jsonValue{}, err
			}
			if p.consume(']') {
				array.close = p.pos - 1
				array.end = p.pos
				return array, nil
			}
			continue
		}
		if !p.consume(']') {
			return jsonValue{}, p.errorf("expected ',' or ']' after array element")
		}
		array.close = p.pos - 1
		array.end = p.pos
		return array, nil
	}
}

func (p *jsoncParser) parseScalar() (jsonValue, error) {
	start := p.pos
	for p.pos < len(p.src) && !isValueDelimiter(p.src[p.pos]) {
		p.pos++
	}
	if start == p.pos {
		return jsonValue{}, p.errorf("expected a JSON value")
	}
	token := p.src[start:p.pos]
	if !json.Valid(token) {
		return jsonValue{}, fmt.Errorf("invalid JSONC at byte %d: invalid scalar", start)
	}
	return jsonValue{kind: kindScalar, start: start, end: p.pos}, nil
}

func (p *jsoncParser) parseString() (string, error) {
	start := p.pos
	p.pos++
	escaped := false
	for p.pos < len(p.src) {
		b := p.src[p.pos]
		p.pos++
		if escaped {
			escaped = false
			continue
		}
		if b == '\\' {
			escaped = true
			continue
		}
		if b == '"' {
			var value string
			if err := json.Unmarshal(p.src[start:p.pos], &value); err != nil {
				return "", fmt.Errorf("invalid JSONC at byte %d: invalid string", start)
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("invalid JSONC at byte %d: unterminated string", start)
}

func (p *jsoncParser) skipTrivia() error {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		case '/':
			if p.pos+1 >= len(p.src) {
				return p.errorf("unexpected '/'")
			}
			switch p.src[p.pos+1] {
			case '/':
				p.pos += 2
				for p.pos < len(p.src) && p.src[p.pos] != '\n' {
					p.pos++
				}
			case '*':
				start := p.pos
				p.pos += 2
				end := bytes.Index(p.src[p.pos:], []byte("*/"))
				if end < 0 {
					return fmt.Errorf("invalid JSONC at byte %d: unterminated block comment", start)
				}
				p.pos += end + 2
			default:
				return p.errorf("unexpected '/'")
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *jsoncParser) consume(want byte) bool {
	if p.pos >= len(p.src) || p.src[p.pos] != want {
		return false
	}
	p.pos++
	return true
}

func (p *jsoncParser) errorf(format string, args ...any) error {
	return fmt.Errorf("invalid JSONC at byte %d: %s", p.pos, fmt.Sprintf(format, args...))
}

func isValueDelimiter(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', ']', '}', '/':
		return true
	default:
		return false
	}
}

func duplicateMember(object jsonValue, key string) (int, bool) {
	index := -1
	for i, member := range object.members {
		if member.key != key {
			continue
		}
		if index >= 0 {
			return -1, true
		}
		index = i
	}
	return index, false
}

func validateUniqueObjectKeys(value jsonValue) error {
	switch value.kind {
	case kindObject:
		seen := make(map[string]struct{}, len(value.members))
		for _, member := range value.members {
			if _, exists := seen[member.key]; exists {
				return fmt.Errorf("provider JSON contains duplicate object key %q", member.key)
			}
			seen[member.key] = struct{}{}
			if err := validateUniqueObjectKeys(member.value); err != nil {
				return err
			}
		}
	case kindArray:
		for _, item := range value.items {
			if err := validateUniqueObjectKeys(item); err != nil {
				return err
			}
		}
	}
	return nil
}
