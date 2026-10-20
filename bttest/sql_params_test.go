// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package bttest

import (
	"context"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

var (
	bytesType  = &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{}}}
	stringType = &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{}}}
	int64Type  = &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{}}}
)

func bytesValue(b string) *btpb.Value {
	return &btpb.Value{Type: bytesType, Kind: &btpb.Value_BytesValue{BytesValue: []byte(b)}}
}

func stringValue(s string) *btpb.Value {
	return &btpb.Value{Type: stringType, Kind: &btpb.Value_StringValue{StringValue: s}}
}

func (f *sqlFixture) prepareParams(ctx context.Context, sql string, types map[string]*btpb.Type) ([]byte, error) {
	resp, err := btpb.NewBigtableClient(f.conn).PrepareQuery(ctx, &btpb.PrepareQueryRequest{InstanceName: "projects/p/instances/i", Query: sql, ParamTypes: types})
	return resp.GetPreparedQuery(), err
}

func (f *sqlFixture) queryValues(ctx context.Context, t *testing.T, sql string, types map[string]*btpb.Type, params map[string]*btpb.Value) []*btpb.Value {
	t.Helper()
	prepared, err := f.prepareParams(ctx, sql, types)
	if err != nil {
		t.Fatalf("PrepareQuery(%q): %v", sql, err)
	}
	msgs, err := f.executeParams(ctx, t, prepared, params)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	var values []*btpb.Value
	for _, m := range msgs {
		var rows btpb.ProtoRows
		if err := proto.Unmarshal(m.GetResults().GetProtoRowsBatch().GetBatchData(), &rows); err != nil {
			t.Fatal(err)
		}
		values = append(values, rows.Values...)
	}
	return values
}

func wantValues(t *testing.T, want, got []*btpb.Value) {
	t.Helper()
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("values (-want +got):\n%s", d)
	}
}

func TestSQLExecuteQueryFindsAParamKeyInAnyCase(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.queryValues(ctx, t, "SELECT _key FROM t WHERE _key = @k", map[string]*btpb.Type{"k": bytesType}, map[string]*btpb.Value{"K": bytesValue("c")})

	wantValues(t, []*btpb.Value{{Kind: &btpb.Value_BytesValue{BytesValue: []byte("c")}}}, got)
}

// Production reads one of the keys at random. The emulator reads the same one on every run.
func TestSQLExecuteQueryReadsTheSameKeyOnEveryRunWhenKeysDifferOnlyInCase(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	seen := map[string]bool{}
	for range 30 {
		got := f.queryValues(ctx, t, "SELECT @s AS v FROM t WHERE _key = 'a'", map[string]*btpb.Type{"s": stringType},
			map[string]*btpb.Value{"S": stringValue("x"), "s": stringValue("y")})
		if len(got) != 1 {
			t.Fatalf("got %d values, want 1", len(got))
		}
		seen[got[0].GetStringValue()] = true
	}

	if len(seen) != 1 {
		t.Errorf("got %d values over 30 runs, want 1: %v", len(seen), seen)
	}
}

func TestSQLExecuteQueryIgnoresAnUndeclaredParam(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.queryValues(ctx, t, "SELECT _key FROM t WHERE _key = @k", map[string]*btpb.Type{"k": bytesType}, map[string]*btpb.Value{"k": bytesValue("c"), "m": bytesValue("a")})

	wantValues(t, []*btpb.Value{{Kind: &btpb.Value_BytesValue{BytesValue: []byte("c")}}}, got)
}

func TestSQLExecuteQueryReadsAValueOfAnotherKindAsTheDeclaredTypesZero(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	n := f.queryValues(ctx, t, "SELECT @n AS v FROM t WHERE _key = 'a'", map[string]*btpb.Type{"n": int64Type},
		map[string]*btpb.Value{"n": {Type: int64Type, Kind: &btpb.Value_StringValue{StringValue: "3"}}})
	s := f.queryValues(ctx, t, "SELECT @s AS v FROM t WHERE _key = 'a'", map[string]*btpb.Type{"s": stringType},
		map[string]*btpb.Value{"s": {Type: stringType, Kind: &btpb.Value_IntValue{IntValue: 3}}})

	wantValues(t, []*btpb.Value{{Kind: &btpb.Value_IntValue{IntValue: 0}}}, n)
	wantValues(t, []*btpb.Value{{Kind: &btpb.Value_StringValue{StringValue: ""}}}, s)
}

func TestSQLPrepareQueryRejectsParamNamesThatDifferOnlyInCase(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	_, err := f.prepareParams(ctx, "SELECT _key FROM t WHERE _key = @k", map[string]*btpb.Type{"k": bytesType, "K": bytesType})

	wantStatus(t, err, codes.InvalidArgument, "Duplicate parameter name k")
}

// Production names one of the bad params at random. The emulator names the same one on every run.
func TestSQLExecuteQueryNamesTheSameBadParamOnEveryRun(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	prepared, err := f.prepareParams(ctx, "SELECT _key FROM t WHERE _key = @b OR _key = @c OR _key = @a", map[string]*btpb.Type{"a": bytesType, "b": bytesType, "c": bytesType})
	if err != nil {
		t.Fatal(err)
	}

	for name, params := range map[string]map[string]*btpb.Value{
		"all missing":   {},
		"all no type":   {"a": {}, "b": {}, "c": {}},
		"all mistyped":  {"a": stringValue("x"), "b": stringValue("x"), "c": stringValue("x")},
		"each bad kind": {"a": {}, "b": stringValue("x")},
	} {
		t.Run(name, func(t *testing.T) {
			seen := map[string]bool{}
			for range 30 {
				_, err := f.executeParams(ctx, t, prepared, params)
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("ExecuteQuery: %v, want InvalidArgument", err)
				}
				seen[status.Convert(err).Message()] = true
			}
			if len(seen) != 1 {
				t.Errorf("got %d messages over 30 runs, want 1: %v", len(seen), seen)
			}
		})
	}
}
