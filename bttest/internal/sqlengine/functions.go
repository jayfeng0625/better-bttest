// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"rsc.io/binaryregexp"
)

// function compiles a call to the function go-googlesql resolved as name, such as $equal or starts_with. It
// returns nil for a function the engine does not support.
func function(name string, xs []expr, ts []Type) expr {
	// A strict function returns NULL when any argument is NULL.
	strict := func(f func(args []Value) (Value, error)) expr {
		return func(x *execCtx, row []Value) (Value, error) {
			args := make([]Value, len(xs))
			for i, a := range xs {
				v, err := a(x, row)
				if err != nil {
					return Value{}, err
				}
				if v.Null {
					return null, nil
				}
				args[i] = v
			}
			return f(args)
		}
	}
	cmp := func(ok func(int) bool) expr {
		return strict(func(a []Value) (Value, error) { return Value{Bool: ok(compare(ts[0].Kind, a[0], a[1]))}, nil })
	}
	switch name {
	case "$equal":
		return cmp(func(c int) bool { return c == 0 })
	case "$not_equal":
		return cmp(func(c int) bool { return c != 0 })
	case "$less":
		return cmp(func(c int) bool { return c < 0 })
	case "$less_or_equal":
		return cmp(func(c int) bool { return c <= 0 })
	case "$greater":
		return cmp(func(c int) bool { return c > 0 })
	case "$greater_or_equal":
		return cmp(func(c int) bool { return c >= 0 })
	case "$between":
		return strict(func(a []Value) (Value, error) {
			k := ts[0].Kind
			return Value{Bool: compare(k, a[1], a[0]) <= 0 && compare(k, a[0], a[2]) <= 0}, nil
		})
	case "$and", "$or":
		return logical(name == "$and", xs)
	case "$not":
		return strict(func(a []Value) (Value, error) { return Value{Bool: !a[0].Bool}, nil })
	case "$is_null":
		return func(x *execCtx, row []Value) (Value, error) {
			v, err := xs[0](x, row)
			return Value{Bool: v.Null}, err
		}
	case "$in":
		return in(xs, ts[0].Kind)
	case "$like":
		return strict(func(a []Value) (Value, error) {
			ok, err := like(a[0].Bytes, a[1].Bytes, ts[0].Kind == KindString)
			return Value{Bool: ok}, err
		})
	case "starts_with":
		return strict(func(a []Value) (Value, error) { return Value{Bool: bytes.HasPrefix(a[0].Bytes, a[1].Bytes)}, nil })
	case "$subscript":
		if ts[0].Kind != KindMap {
			return nil
		}
		return strict(func(a []Value) (Value, error) {
			m, k := a[0], a[1]
			if i, ok := slices.BinarySearchFunc(m.Keys, k, func(e, k Value) int { return bytes.Compare(e.Bytes, k.Bytes) }); ok {
				return m.Vals[i], nil
			}
			return null, nil
		})
	case toInt64:
		return strict(func(a []Value) (Value, error) {
			if len(a[0].Bytes) != 8 {
				return Value{}, status.Errorf(codes.InvalidArgument, "incorrect value size. expected: 8 bytes, actual: %d bytes", len(a[0].Bytes))
			}
			return Value{Int: int64(binary.BigEndian.Uint64(a[0].Bytes))}, nil
		})
	}
	return nil
}

// logical compiles AND and OR with three-valued logic: a deciding operand wins over NULL.
func logical(isAnd bool, xs []expr) expr {
	return func(x *execCtx, row []Value) (Value, error) {
		sawNull := false
		for _, a := range xs {
			v, err := a(x, row)
			if err != nil {
				return Value{}, err
			}
			switch {
			case v.Null:
				sawNull = true
			case v.Bool != isAnd:
				return Value{Bool: !isAnd}, nil
			}
		}
		if sawNull {
			return null, nil
		}
		return Value{Bool: isAnd}, nil
	}
}

// in compiles x IN (a, b, ...): true on a match, else NULL when x or any candidate is NULL, else false.
func in(xs []expr, k Kind) expr {
	return func(x *execCtx, row []Value) (Value, error) {
		v, err := xs[0](x, row)
		if err != nil || v.Null {
			return null, err
		}
		sawNull := false
		for _, a := range xs[1:] {
			w, err := a(x, row)
			if err != nil {
				return Value{}, err
			}
			if w.Null {
				sawNull = true
			} else if compare(k, v, w) == 0 {
				return Value{Bool: true}, nil
			}
		}
		if sawNull {
			return null, nil
		}
		return Value{Bool: false}, nil
	}
}

// like matches s against a LIKE pattern, where % matches any sequence, _ matches one character of a STRING or
// one byte of BYTES, and a backslash makes the next character literal.
func like(s, pattern []byte, isString bool) (bool, error) {
	var re strings.Builder
	re.WriteString(`(?s)\A`)
	quote := binaryregexp.QuoteMeta
	if isString {
		quote = regexp.QuoteMeta
	}
	for len(pattern) > 0 {
		n := 1
		if isString {
			_, n = utf8.DecodeRune(pattern)
		}
		c := pattern[:n]
		pattern = pattern[n:]
		switch string(c) {
		case `%`:
			re.WriteString(`.*`)
		case `_`:
			re.WriteString(`.`)
		case `\`:
			if len(pattern) == 0 {
				return false, status.Error(codes.OutOfRange, "LIKE pattern ends with a backslash")
			}
			n = 1
			if isString {
				_, n = utf8.DecodeRune(pattern)
			}
			re.WriteString(quote(string(pattern[:n])))
			pattern = pattern[n:]
		default:
			re.WriteString(quote(string(c)))
		}
	}
	re.WriteString(`\z`)
	if isString {
		return regexp.MustCompile(re.String()).Match(s), nil
	}
	return binaryregexp.MustCompile(re.String()).Match(s), nil
}

// castExpr compiles CAST between BYTES and STRING, and to a value's own type.
func castExpr(x expr, from, to Type) (expr, error) {
	switch {
	case from.Kind == to.Kind && from.Kind != KindMap:
		return x, nil
	case from.Kind == KindString && to.Kind == KindBytes:
		return x, nil
	case from.Kind == KindBytes && to.Kind == KindString:
		return func(xc *execCtx, row []Value) (Value, error) {
			v, err := x(xc, row)
			if err != nil || v.Null {
				return v, err
			}
			if !utf8.Valid(v.Bytes) {
				return Value{}, status.Error(codes.OutOfRange, "Invalid cast of bytes to UTF8 string")
			}
			return v, nil
		}, nil
	}
	return nil, unsupported("CAST to " + kindName(to.Kind) + " from " + kindName(from.Kind))
}

func kindName(k Kind) string {
	switch k {
	case KindBytes:
		return "BYTES"
	case KindString:
		return "STRING"
	case KindInt64:
		return "INT64"
	case KindBool:
		return "BOOL"
	}
	return "MAP"
}
