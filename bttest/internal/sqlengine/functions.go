// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
	"encoding/binary"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"rsc.io/binaryregexp"
)

// function compiles a call to the function go-googlesql resolved as name, such as $equal or starts_with. consts
// holds the value of each argument that is a literal, and nil for the others. It returns nil for a function the
// engine does not support.
func function(name string, xs []expr, ts []Type, consts []*Value) expr {
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
		// GoogleSQL defines v BETWEEN lo AND hi as lo <= v AND v <= hi, so a NULL bound yields false when the other
		// comparison is false.
		le := func(i, j int) expr {
			return function("$less_or_equal", []expr{xs[i], xs[j]}, []Type{ts[i], ts[j]}, []*Value{consts[i], consts[j]})
		}
		return logical(true, []expr{le(1, 0), le(0, 2)})
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
		isString := ts[0].Kind == KindString
		pattern := func(a []Value) (func([]byte) bool, error) { return like(a[1].Bytes, isString) }
		if p := consts[1]; p != nil {
			match, err := like(p.Bytes, isString)
			pattern = func([]Value) (func([]byte) bool, error) { return match, err }
		}
		return strict(func(a []Value) (Value, error) {
			match, err := pattern(a)
			if err != nil {
				return Value{}, err
			}
			return Value{Bool: match(a[0].Bytes)}, nil
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
	case "$subtract":
		if ts[0].Kind != KindInt64 {
			return nil
		}
		return strict(func(a []Value) (Value, error) {
			x, y := a[0].Int, a[1].Int
			r := x - y
			if (y > 0 && r > x) || (y < 0 && r < x) {
				// The message is an assumption: no production overflow of - was recorded.
				return Value{}, status.Errorf(codes.OutOfRange, "int64 overflow: %d - %d", x, y)
			}
			return Value{Int: r}, nil
		})
	case "div":
		if ts[0].Kind != KindInt64 {
			return nil
		}
		return strict(func(a []Value) (Value, error) {
			x, y := a[0].Int, a[1].Int
			switch {
			case y == 0:
				return Value{}, status.Error(codes.OutOfRange, "division by zero: DIV")
			case x == math.MinInt64 && y == -1:
				// The message is an assumption: no production overflow of DIV was recorded.
				return Value{}, status.Errorf(codes.OutOfRange, "int64 overflow: DIV(%d, %d)", x, y)
			}
			return Value{Int: x / y}, nil
		})
	case "$array_at_offset":
		return strict(func(a []Value) (Value, error) {
			arr, i := a[0].Elems, a[1].Int
			if i < 0 {
				return Value{}, status.Error(codes.OutOfRange, "Array index is out of bounds")
			}
			if i >= int64(len(arr)) {
				return Value{}, status.Errorf(codes.OutOfRange, "Array index %d is out of bounds", i)
			}
			return arr[i], nil
		})
	case "split":
		// The engine supports SPLIT only on BYTES.
		if ts[0].Kind != KindBytes {
			return nil
		}
		return strict(func(a []Value) (Value, error) { return splitBytes(a[0].Bytes, a[1].Bytes), nil })
	case "json_query_array":
		// go-googlesql passes the default path $ when the query omits it. compiler.call rejects any other path.
		return strict(func(a []Value) (Value, error) { return jsonQueryArray(a[0].Bytes), nil })
	case "array_concat":
		return strict(func(a []Value) (Value, error) {
			out := Value{Elems: []Value{}}
			for _, v := range a {
				out.Elems = append(out.Elems, v.Elems...)
			}
			return out, nil
		})
	case "$make_array":
		return func(x *execCtx, row []Value) (Value, error) {
			out := Value{Elems: make([]Value, len(xs))}
			for i, a := range xs {
				v, err := a(x, row)
				if err != nil {
					return Value{}, err
				}
				out.Elems[i] = v
			}
			return out, nil
		}
	case "coalesce":
		return func(x *execCtx, row []Value) (Value, error) {
			for _, a := range xs {
				v, err := a(x, row)
				if err != nil || !v.Null {
					return v, err
				}
			}
			return null, nil
		}
	case "$case_no_value":
		// CASE WHEN c1 THEN v1 ... [ELSE e] END resolves to its conditions and values in pairs, then the ELSE value
		// when the count is odd. The engine evaluates only the chosen value.
		return func(x *execCtx, row []Value) (Value, error) {
			n := len(xs)
			for i := 0; i+1 < n; i += 2 {
				c, err := xs[i](x, row)
				if err != nil {
					return Value{}, err
				}
				if !c.Null && c.Bool {
					return xs[i+1](x, row)
				}
			}
			if n%2 == 1 {
				return xs[n-1](x, row)
			}
			return null, nil
		}
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

// splitBytes splits s at each d. An empty d splits s into single bytes. An empty s gives one empty element.
func splitBytes(s, d []byte) Value {
	out := Value{Elems: []Value{}}
	if len(s) == 0 {
		out.Elems = append(out.Elems, Value{Bytes: []byte{}})
		return out
	}
	if len(d) == 0 {
		for i := range s {
			out.Elems = append(out.Elems, Value{Bytes: s[i : i+1]})
		}
		return out
	}
	for _, p := range bytes.Split(s, d) {
		out.Elems = append(out.Elems, Value{Bytes: p})
	}
	return out
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

// like compiles a LIKE pattern into a matcher, where % matches any sequence, _ matches one character of a STRING
// or one byte of BYTES, and a backslash makes the next character literal.
func like(pattern []byte, isString bool) (func([]byte) bool, error) {
	var re strings.Builder
	re.WriteString(`(?s)\A`)
	quote := binaryregexp.QuoteMeta
	if isString {
		quote = regexp.QuoteMeta
	}
	next := func() string {
		n := 1
		if isString {
			_, n = utf8.DecodeRune(pattern)
		}
		c := pattern[:n]
		pattern = pattern[n:]
		return string(c)
	}
	for len(pattern) > 0 {
		switch c := next(); c {
		case `%`:
			re.WriteString(`.*`)
		case `_`:
			re.WriteString(`.`)
		case `\`:
			if len(pattern) == 0 {
				return nil, status.Error(codes.OutOfRange, "LIKE pattern ends with a backslash")
			}
			re.WriteString(quote(next()))
		default:
			re.WriteString(quote(c))
		}
	}
	re.WriteString(`\z`)
	if isString {
		return regexp.MustCompile(re.String()).Match, nil
	}
	return binaryregexp.MustCompile(re.String()).Match, nil
}

// castExpr compiles CAST between BYTES and STRING, and to a value's own type.
func castExpr(x expr, from, to Type) (expr, error) {
	switch {
	case from.Kind == to.Kind && from.Kind != KindMap && from.Kind != KindArray:
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
	case KindArray:
		return "ARRAY"
	}
	return "MAP"
}
