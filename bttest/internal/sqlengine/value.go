// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// Kind is a SQL type kind the engine supports.
type Kind int

const (
	KindBytes Kind = iota + 1
	KindString
	KindInt64
	KindBool
	KindMap
	KindArray
)

// Type is a SQL type. Key and Elem are a map's key and value types, and Elem is an array's element type.
type Type struct {
	Kind Kind
	Key  *Type
	Elem *Type
}

// Value is one SQL value. Null marks NULL of any type.
type Value struct {
	Null  bool
	Bytes []byte // BYTES and STRING
	Int   int64
	Bool  bool
	Keys  []Value // MAP keys, in ascending order
	Vals  []Value // MAP values, by Keys index
	Elems []Value // ARRAY elements
}

var null = Value{Null: true}

// TypeFromProto converts a query parameter type. The engine accepts BYTES, STRING and INT64 parameters.
func TypeFromProto(t *btpb.Type) (Type, error) {
	switch t.GetKind().(type) {
	case *btpb.Type_BytesType:
		return Type{Kind: KindBytes}, nil
	case *btpb.Type_StringType:
		return Type{Kind: KindString}, nil
	case *btpb.Type_Int64Type:
		return Type{Kind: KindInt64}, nil
	}
	return Type{}, unsupported("query parameters of type " + TypeName(t))
}

// TypeName names a type by its oneof field, as production's messages do.
func TypeName(t *btpb.Type) string {
	if f := t.ProtoReflect().WhichOneof(t.ProtoReflect().Descriptor().Oneofs().ByName("kind")); f != nil {
		return string(f.Name())
	}
	return "unknown_type"
}

// Proto converts the type into result metadata.
func (t Type) Proto() *btpb.Type {
	switch t.Kind {
	case KindBytes:
		return &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{}}}
	case KindString:
		return &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{}}}
	case KindInt64:
		return &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{}}}
	case KindBool:
		return &btpb.Type{Kind: &btpb.Type_BoolType{BoolType: &btpb.Type_Bool{}}}
	case KindMap:
		return &btpb.Type{Kind: &btpb.Type_MapType{MapType: &btpb.Type_Map{KeyType: t.Key.Proto(), ValueType: t.Elem.Proto()}}}
	case KindArray:
		return &btpb.Type{Kind: &btpb.Type_ArrayType{ArrayType: &btpb.Type_Array{ElementType: t.Elem.Proto()}}}
	}
	panic(fmt.Sprintf("sqlengine: no proto for kind %d", t.Kind))
}

// ValueFromProto converts a query parameter value of type t. A value with no kind is NULL.
func ValueFromProto(v *btpb.Value, t Type) Value {
	if v.GetKind() == nil {
		return null
	}
	switch t.Kind {
	case KindBytes:
		return Value{Bytes: v.GetBytesValue()}
	case KindString:
		return Value{Bytes: []byte(v.GetStringValue())}
	case KindInt64:
		return Value{Int: v.GetIntValue()}
	}
	return null
}

// Proto converts a value of type t into a ProtoRows value. NULL is a value with no kind. A map is an array of
// two-element arrays, each holding a key and its value.
func (v Value) Proto(t Type) *btpb.Value {
	if v.Null {
		return &btpb.Value{}
	}
	switch t.Kind {
	case KindBytes:
		return &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: v.Bytes}}
	case KindString:
		return &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: string(v.Bytes)}}
	case KindInt64:
		return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: v.Int}}
	case KindBool:
		return &btpb.Value{Kind: &btpb.Value_BoolValue{BoolValue: v.Bool}}
	case KindMap:
		a := &btpb.ArrayValue{Values: make([]*btpb.Value, len(v.Keys))}
		for i := range v.Keys {
			a.Values[i] = &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{
				Values: []*btpb.Value{v.Keys[i].Proto(*t.Key), v.Vals[i].Proto(*t.Elem)},
			}}}
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: a}}
	case KindArray:
		a := &btpb.ArrayValue{Values: make([]*btpb.Value, len(v.Elems))}
		for i, e := range v.Elems {
			a.Values[i] = e.Proto(*t.Elem)
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: a}}
	}
	panic(fmt.Sprintf("sqlengine: no proto for kind %d", t.Kind))
}

// compare orders two non-NULL values of kind k: BYTES and STRING by their bytes, INT64 numerically, and false
// before true.
func compare(k Kind, a, b Value) int {
	switch k {
	case KindBytes, KindString:
		return bytes.Compare(a.Bytes, b.Bytes)
	case KindInt64:
		return cmp.Compare(a.Int, b.Int)
	case KindBool:
		switch {
		case a.Bool == b.Bool:
			return 0
		case !a.Bool:
			return -1
		}
		return 1
	}
	panic(fmt.Sprintf("sqlengine: cannot compare kind %d", k))
}

// newMap builds a map value with BYTES keys from unsorted entries.
func newMap(entries map[string]Value) Value {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	m := Value{Keys: make([]Value, len(keys)), Vals: make([]Value, len(keys))}
	for i, k := range keys {
		m.Keys[i] = Value{Bytes: []byte(k)}
		m.Vals[i] = entries[k]
	}
	return m
}

// appendGroupKey appends an encoding of v that equals another value's encoding exactly when GROUP BY puts the
// two values in one group. NULL forms its own group.
func appendGroupKey(b []byte, v Value) []byte {
	if v.Null {
		return append(b, 0)
	}
	b = append(b, 1)
	b = binary.AppendUvarint(b, uint64(len(v.Bytes)))
	b = append(b, v.Bytes...)
	b = binary.BigEndian.AppendUint64(b, uint64(v.Int))
	if v.Bool {
		return append(b, 1)
	}
	return append(b, 0)
}
