// SPDX-License-Identifier: Apache-2.0

// Package sqlengine runs Bigtable's GoogleSQL queries over one table. go-googlesql parses and types each query
// against a catalog built from the table's families, and the engine compiles the resolved tree into Go operators
// that read rows through a Source.
package sqlengine

import (
	"fmt"
	"sync"

	gsql "github.com/goccy/go-googlesql"
)

// Family is one column family as SQL sees it. Int64 marks a Sum, Min or Max family, whose values SQL types as
// INT64.
type Family struct {
	Name  string
	Int64 bool
}

// Table is one table as SQL sees it. SELECT * lists the families in the order given.
type Table struct {
	Name     string
	Families []Family
}

var (
	initOnce sync.Once
	initErr  error
)

// features are the language features Bigtable's dialect needs beyond the defaults.
var features = []gsql.LanguageFeature{
	gsql.LanguageFeatureFeatureMapType,
	gsql.LanguageFeatureFeatureAllowDashesInTableName,
	gsql.LanguageFeatureFeatureImplicitCoercionStringLiteralToBytes,
	gsql.LanguageFeatureFeatureBareArrayAccess,
	gsql.LanguageFeatureFeatureJsonArrayFunctions,
}

// env is one analyzer setup. go-googlesql's AnalyzerOptions must not be shared across concurrent analyses, so
// each Prepare builds its own.
type env struct {
	cat  *gsql.SimpleCatalog
	opts *gsql.AnalyzerOptions
	tf   *gsql.TypeFactory
	lang *gsql.LanguageOptions
}

// newEnv builds an analyzer setup with no tables. Prepare adds each table the query names.
func newEnv(params map[string]Type) (*env, error) {
	initOnce.Do(func() { initErr = gsql.Init() })
	if initErr != nil {
		return nil, initErr
	}
	tf, err := gsql.NewTypeFactory()
	if err != nil {
		return nil, err
	}
	lang, err := gsql.NewLanguageOptions()
	if err != nil {
		return nil, err
	}
	for _, f := range features {
		if err := lang.EnableLanguageFeature(f); err != nil {
			return nil, err
		}
	}
	if err := lang.SetProductMode(gsql.ProductModeProductExternal); err != nil {
		return nil, err
	}
	cat, err := gsql.NewSimpleCatalog("bigtable", tf)
	if err != nil {
		return nil, err
	}
	if err := cat.AddBuiltinFunctionsAndTypes(&gsql.BuiltinFunctionOptions{LanguageOptions: lang}); err != nil {
		return nil, err
	}
	e := &env{cat: cat, tf: tf, lang: lang}
	if err := e.addToInt64(); err != nil {
		return nil, err
	}
	opts, err := gsql.NewAnalyzerOptions2()
	if err != nil {
		return nil, err
	}
	if err := opts.SetLanguage(lang); err != nil {
		return nil, err
	}
	if err := opts.SetErrorMessageMode(gsql.ErrorMessageModeErrorMessageOneLine); err != nil {
		return nil, err
	}
	// A table scan then lists only the columns the query reads, which tells which families a plan depends on.
	if err := opts.SetPruneUnusedColumns(true); err != nil {
		return nil, err
	}
	for name, t := range params {
		gt, err := e.gsqlType(t)
		if err != nil {
			return nil, err
		}
		if err := opts.AddQueryParameter(name, gt); err != nil {
			return nil, err
		}
	}
	e.opts = opts
	return e, nil
}

// addTable adds a table with a _key column and one map column per family. The analyzer matches column names
// case-insensitively, and Bigtable allows families that differ only in case, so the table allows duplicate names.
func (e *env) addTable(t Table) error {
	st, err := gsql.NewSimpleTable(t.Name, 0)
	if err != nil {
		return err
	}
	if err := st.SetAllowDuplicateColumnNames(true); err != nil {
		return err
	}
	add := func(name string, ct Type) error {
		gt, err := e.gsqlType(ct)
		if err != nil {
			return err
		}
		col, err := gsql.NewSimpleColumn(t.Name, name, gt, false, false)
		if err != nil {
			return err
		}
		return st.AddColumn2(col, false)
	}
	if err := add("_key", Type{Kind: KindBytes}); err != nil {
		return err
	}
	for _, f := range t.Families {
		if err := add(f.Name, familyType(f)); err != nil {
			return err
		}
	}
	return e.cat.AddTable(st)
}

// familyType is a family's SQL type: a map from qualifier to the newest value.
func familyType(f Family) Type {
	v := KindBytes
	if f.Int64 {
		v = KindInt64
	}
	return Type{Kind: KindMap, Key: &Type{Kind: KindBytes}, Elem: &Type{Kind: v}}
}

// addToInt64 registers Bigtable's TO_INT64, which reads 8 big-endian bytes as an INT64.
func (e *env) addToInt64() error {
	i64, err := e.tf.GetInt64()
	if err != nil {
		return err
	}
	b, err := e.tf.GetBytes()
	if err != nil {
		return err
	}
	res, err := gsql.NewFunctionArgumentType3(i64, 1)
	if err != nil {
		return err
	}
	arg, err := gsql.NewFunctionArgumentType3(b, 1)
	if err != nil {
		return err
	}
	sig, err := gsql.NewFunctionSignature3(res, []*gsql.FunctionArgumentType{arg}, 0)
	if err != nil {
		return err
	}
	fn, err := gsql.NewFunction4(toInt64, "bigtable", gsql.FunctionEnums_ModeScalar, []*gsql.FunctionSignature{sig})
	if err != nil {
		return err
	}
	return e.cat.AddFunction(fn)
}

const toInt64 = "TO_INT64"

func (e *env) gsqlType(t Type) (gsql.Googlesql_TypeNode, error) {
	switch t.Kind {
	case KindBytes:
		return e.tf.GetBytes()
	case KindString:
		return e.tf.GetString()
	case KindInt64:
		return e.tf.GetInt64()
	case KindBool:
		return e.tf.GetBool()
	case KindMap:
		k, err := e.gsqlType(*t.Key)
		if err != nil {
			return nil, err
		}
		v, err := e.gsqlType(*t.Elem)
		if err != nil {
			return nil, err
		}
		return e.tf.MakeMapType2(k, v, e.lang)
	}
	return nil, fmt.Errorf("no analyzer type for kind %d", t.Kind)
}

// engineType converts an analyzer type into the engine's Type. It returns InvalidArgument for a type the engine
// does not support.
func engineType(t gsql.Googlesql_TypeNode) (Type, error) {
	k, err := t.Kind()
	if err != nil {
		return Type{}, internal(err)
	}
	switch k {
	case gsql.TypeKindTypeBytes:
		return Type{Kind: KindBytes}, nil
	case gsql.TypeKindTypeString:
		return Type{Kind: KindString}, nil
	case gsql.TypeKindTypeInt64:
		return Type{Kind: KindInt64}, nil
	case gsql.TypeKindTypeBool:
		return Type{Kind: KindBool}, nil
	case gsql.TypeKindTypeMap:
		mt, err := t.AsMap()
		if err != nil {
			return Type{}, internal(err)
		}
		kt, err := mt.KeyType()
		if err != nil {
			return Type{}, internal(err)
		}
		vt, err := mt.ValueType()
		if err != nil {
			return Type{}, internal(err)
		}
		kk, err := engineType(kt)
		if err != nil {
			return Type{}, err
		}
		vv, err := engineType(vt)
		if err != nil {
			return Type{}, err
		}
		return Type{Kind: KindMap, Key: &kk, Elem: &vv}, nil
	}
	name, err := t.DebugString(false)
	if err != nil {
		return Type{}, internal(err)
	}
	return Type{}, unsupported("values of type " + name)
}
