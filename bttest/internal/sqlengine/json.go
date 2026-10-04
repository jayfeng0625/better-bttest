// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// jsonQueryArray is JSON_QUERY_ARRAY(json) as production answered it on 2026-10-03. It returns each element as
// compact JSON text. A string keeps its quotes, and jsonQueryArray re-escapes the string. Number text stays as
// written. An object keeps its key order and duplicate keys. A JSON null becomes the text null.
// Invalid JSON and a value that is not an array give NULL.
func jsonQueryArray(text []byte) Value {
	d := json.NewDecoder(bytes.NewReader(text))
	d.UseNumber()
	if t, err := d.Token(); err != nil || t != json.Delim('[') {
		return null
	}
	out := Value{Elems: []Value{}}
	for d.More() {
		b, err := appendJSONValue(nil, d)
		if err != nil {
			return null
		}
		out.Elems = append(out.Elems, Value{Bytes: b})
	}
	if _, err := d.Token(); err != nil {
		return null
	}
	if _, err := d.Token(); err != io.EOF {
		return null
	}
	return out
}

func appendJSONValue(b []byte, d *json.Decoder) ([]byte, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case string:
		return appendJSONString(b, t), nil
	case json.Number:
		return append(b, t...), nil
	case bool:
		return strconv.AppendBool(b, t), nil
	case nil:
		return append(b, "null"...), nil
	}
	// The token is [ or {. A key is a string token, so the loop writes it the same way as a value.
	open := t.(json.Delim)
	b = append(b, byte(open))
	for i := 0; d.More(); i++ {
		if i > 0 {
			sep := byte(',')
			if open == '{' && i%2 == 1 {
				sep = ':'
			}
			b = append(b, sep)
		}
		if b, err = appendJSONValue(b, d); err != nil {
			return nil, err
		}
	}
	end, err := d.Token()
	if err != nil {
		return nil, err
	}
	return append(b, byte(end.(json.Delim))), nil
}

// appendJSONString escapes a quote, a backslash, and control characters, and writes everything else raw. Production
// escapes a quote, decodes \u0041 and \/, and writes UTF-8 raw. The control-character escapes are an assumption.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if r < 0x20 {
				b = fmt.Appendf(b, `\u%04x`, r)
			} else {
				b = utf8.AppendRune(b, r)
			}
		}
	}
	return append(b, '"')
}
