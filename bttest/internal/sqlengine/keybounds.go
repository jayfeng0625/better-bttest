// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package sqlengine

import (
	gsql "github.com/goccy/go-googlesql"
)

// comparison is which bounds a comparison puts on _key when _key is its left operand.
type comparison struct {
	lower, upper, strict bool
}

var comparisons = map[string]comparison{
	"$equal":            {lower: true, upper: true},
	"$less":             {upper: true, strict: true},
	"$less_or_equal":    {upper: true},
	"$greater":          {lower: true, strict: true},
	"$greater_or_equal": {lower: true},
}

// keyBounds takes the scan's key bounds from the filter's comparisons of _key with a literal or parameter, through
// AND and BETWEEN. Any other construct bounds nothing, so the scan reads every key the filter may pass.
func (c *compiler) keyBounds(ts *tableScan, e gsql.ResolvedExprNode) error {
	call, ok := e.(*gsql.ResolvedFunctionCall)
	if !ok {
		return nil
	}
	fn, err := call.Function()
	if err != nil {
		return internal(err)
	}
	name, err := fn.Name()
	if err != nil {
		return internal(err)
	}
	args, err := call.ArgumentList()
	if err != nil {
		return internal(err)
	}
	if name == "$and" {
		for _, a := range args {
			if err := c.keyBounds(ts, a); err != nil {
				return err
			}
		}
		return nil
	}
	if name == "$between" {
		if isKey, err := ts.isKey(args[0]); err != nil || !isKey {
			return err
		}
		if err := c.keyBound(ts, args[1], comparison{lower: true}); err != nil {
			return err
		}
		return c.keyBound(ts, args[2], comparison{upper: true})
	}
	bounds, ok := comparisons[name]
	if !ok {
		return nil
	}
	keyLeft, err := ts.isKey(args[0])
	if err != nil {
		return err
	}
	keyRight, err := ts.isKey(args[1])
	if err != nil {
		return err
	}
	switch {
	case keyLeft:
		return c.keyBound(ts, args[1], bounds)
	case keyRight:
		return c.keyBound(ts, args[0], comparison{lower: bounds.upper, upper: bounds.lower, strict: bounds.strict})
	}
	return nil
}

func (c *compiler) keyBound(ts *tableScan, e gsql.ResolvedExprNode, cmp comparison) error {
	switch e.(type) {
	case *gsql.ResolvedLiteral, *gsql.ResolvedParameter:
	default:
		return nil
	}
	x, err := c.expr(e)
	if err != nil {
		return err
	}
	b := keyBound{value: x, strict: cmp.strict}
	if cmp.lower {
		ts.lower = append(ts.lower, b)
	}
	if cmp.upper {
		ts.upper = append(ts.upper, b)
	}
	return nil
}

func (ts *tableScan) isKey(e gsql.ResolvedExprNode) (bool, error) {
	ref, ok := e.(*gsql.ResolvedColumnRef)
	if !ok {
		return false, nil
	}
	col, err := ref.Column()
	if err != nil {
		return false, internal(err)
	}
	id, _, err := colInfo(col)
	if err != nil {
		return false, err
	}
	for _, c := range ts.cols {
		if c.family == nil && c.id == id {
			return true, nil
		}
	}
	return false, nil
}
