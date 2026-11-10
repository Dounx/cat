package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// decode parses a response body. A body that is not JSON is compared as one
// string, so plain-text endpoints can be watched too.
func decode(body []byte) (v any, isJSON bool) {
	body = bytes.TrimSpace(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")))
	if json.Valid(body) {
		// Numbers keep their literal form: large ids must not lose digits.
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if dec.Decode(&v) == nil {
			return v, true
		}
	}
	return strings.ToValidUTF8(string(body), "�"), false
}

// canonical renders a value as compact JSON with sorted object keys, so equal
// values always give the same text.
func canonical(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimRight(b.String(), "\n")
}

// parsePath splits a field path such as "data.items[0].name" into segments.
// An array index may also be written as its own segment ("items.0.name").
// wildcard allows "*" for "every key or element".
func parsePath(s string, wildcard bool) ([]string, error) {
	orig := s
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "$"), ".")
	bad := func() ([]string, error) { return nil, fmt.Errorf("看不懂的字段路径「%s」", orig) }
	var segs []string
	for _, part := range strings.Split(s, ".") {
		name, rest := part, ""
		if i := strings.IndexByte(part, '['); i >= 0 {
			name, rest = part[:i], part[i:]
		}
		if name == "" && rest == "" {
			return bad()
		}
		if name != "" {
			segs = append(segs, name)
		}
		for rest != "" {
			end := strings.IndexByte(rest, ']')
			if rest[0] != '[' || end < 0 {
				return bad()
			}
			idx := rest[1:end]
			if _, err := strconv.ParseUint(idx, 10, 31); err != nil && idx != "*" {
				return bad()
			}
			segs = append(segs, idx)
			rest = rest[end+1:]
		}
	}
	for _, seg := range segs {
		if seg == "*" && !wildcard {
			return nil, fmt.Errorf("字段路径「%s」里不能用 *", orig)
		}
	}
	return segs, nil
}

// lookup follows a path into a value.
func lookup(v any, path []string) (any, bool) {
	for _, seg := range path {
		switch t := v.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// drop removes what path points at from v and returns the result. A path that
// leads nowhere is not an error: one ignore list serves several APIs.
func drop(v any, path []string) any {
	seg, rest := path[0], path[1:]
	switch t := v.(type) {
	case map[string]any:
		keys := []string{seg}
		if seg == "*" {
			keys = keys[:0]
			for k := range t {
				keys = append(keys, k)
			}
		}
		for _, k := range keys {
			child, ok := t[k]
			if !ok {
				continue
			}
			if len(rest) == 0 {
				delete(t, k)
			} else {
				t[k] = drop(child, rest)
			}
		}
	case []any:
		if seg == "*" {
			if len(rest) == 0 {
				return []any{}
			}
			for i := range t {
				t[i] = drop(t[i], rest)
			}
			return t
		}
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(t) {
			return t
		}
		if len(rest) == 0 {
			return append(t[:i:i], t[i+1:]...)
		}
		t[i] = drop(t[i], rest)
	}
	return v
}

// change is one difference between two responses.
type change struct {
	Path string `json:"path"` // "" when the watched value itself was replaced
	Type string `json:"type"` // "added", "removed" or "changed"
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func joinIndex(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// diff appends the differences between old and cur, both found at path.
func diff(path string, old, cur any, out *[]change) {
	switch a := old.(type) {
	case map[string]any:
		b, ok := cur.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(a)+len(b))
		for k := range a {
			keys = append(keys, k)
		}
		for k := range b {
			if _, dup := a[k]; !dup {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			av, inA := a[k]
			bv, inB := b[k]
			switch {
			case !inA:
				*out = append(*out, change{Path: joinKey(path, k), Type: "added", New: bv})
			case !inB:
				*out = append(*out, change{Path: joinKey(path, k), Type: "removed", Old: av})
			default:
				diff(joinKey(path, k), av, bv, out)
			}
		}
		return
	case []any:
		b, ok := cur.([]any)
		if !ok {
			break
		}
		diffArrays(path, a, b, out)
		return
	}
	if canonical(old) != canonical(cur) {
		*out = append(*out, change{Path: path, Type: "changed", Old: old, New: cur})
	}
}

// diffArrays compares two arrays. Elements are matched by position only where
// that is safe (the lengths agree after setting aside what is equal at both
// ends); otherwise by value, so an element inserted at the front of a list is
// reported as one addition rather than as a change to every element after it.
func diffArrays(path string, a, b []any, out *[]change) {
	ka, kb := make([]string, len(a)), make([]string, len(b))
	for i, v := range a {
		ka[i] = canonical(v)
	}
	for i, v := range b {
		kb[i] = canonical(v)
	}
	lo := 0
	for lo < len(a) && lo < len(b) && ka[lo] == kb[lo] {
		lo++
	}
	ea, eb := len(a), len(b)
	for ea > lo && eb > lo && ka[ea-1] == kb[eb-1] {
		ea, eb = ea-1, eb-1
	}
	if ea-lo == eb-lo {
		for i := lo; i < ea; i++ {
			diff(joinIndex(path, i), a[i], b[i], out)
		}
		return
	}
	inB := map[string]int{}
	for _, k := range kb[lo:eb] {
		inB[k]++
	}
	inA := map[string]int{}
	for i := lo; i < ea; i++ {
		if inB[ka[i]] > 0 {
			inB[ka[i]]--
			inA[ka[i]]++ // matched: not an addition either
		} else {
			*out = append(*out, change{Path: joinIndex(path, i), Type: "removed", Old: a[i]})
		}
	}
	for i := lo; i < eb; i++ {
		if inA[kb[i]] > 0 {
			inA[kb[i]]--
		} else {
			*out = append(*out, change{Path: joinIndex(path, i), Type: "added", New: b[i]})
		}
	}
}
