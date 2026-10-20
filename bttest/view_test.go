// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package bttest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// viewFixture is a sqlFixture with an instance admin client and the items-prod table.
type viewFixture struct {
	*sqlFixture
	iadmin *bigtable.InstanceAdminClient
}

const expiredViewQuery = "SELECT _key AS rowKey FROM `items-prod` WHERE mark['flag'] IS NULL ORDER BY rowKey"

func newViewFixture(ctx context.Context, t testing.TB, rows []itemRow) *viewFixture {
	t.Helper()
	f := newSQLFixture(ctx, t)
	iadmin, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(f.conn))
	if err != nil {
		t.Fatal(err)
	}
	f.createItemsTable(ctx, t, "items-prod", rows)
	return &viewFixture{sqlFixture: f, iadmin: iadmin}
}

func (f *viewFixture) createView(ctx context.Context, t testing.TB, id, query string) {
	t.Helper()
	if err := f.iadmin.CreateMaterializedView(ctx, "i", &bigtable.MaterializedViewInfo{MaterializedViewID: id, Query: query}); err != nil {
		t.Fatalf("CreateMaterializedView(%s): %v", id, err)
	}
}

// Production's error for a view that does not exist, as a probe of production recorded it on 2026-10-04.
func TestGetMissingMaterializedViewReturnsNotFound(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, nil)
	_, err := f.iadmin.MaterializedViewInfo(ctx, "i", "missing")
	wantStatus(t, err, codes.NotFound, "Failed to read: projects/{p}/instances/i/materializedViews/missing")
}

func TestReadRowsOnMissingViewReturnsNotFound(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, nil)
	err := f.client.OpenMaterializedView("missing").ReadRows(ctx, bigtable.InfiniteRange(""), func(bigtable.Row) bool { return true })
	wantStatus(t, err, codes.NotFound, "Not found: projects/p/instances/i/materializedViews/missing")
}

func TestViewAdminRoundTrips(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)

	f.createView(ctx, t, "v_totals", totalsQuery)
	f.createView(ctx, t, "v_expired", expiredViewQuery)

	got, err := f.iadmin.MaterializedViewInfo(ctx, "i", "v_expired")
	if err != nil {
		t.Fatalf("MaterializedViewInfo: %v", err)
	}
	want := &bigtable.MaterializedViewInfo{MaterializedViewID: "v_expired", Query: expiredViewQuery, DeletionProtection: bigtable.Unprotected}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("MaterializedViewInfo (-want +got):\n%s", diff)
	}
	list, err := f.iadmin.MaterializedViews(ctx, "i")
	if err != nil {
		t.Fatalf("MaterializedViews: %v", err)
	}
	wantList := []bigtable.MaterializedViewInfo{
		{MaterializedViewID: "v_expired", Query: expiredViewQuery, DeletionProtection: bigtable.Unprotected},
		{MaterializedViewID: "v_totals", Query: totalsQuery, DeletionProtection: bigtable.Unprotected},
	}
	if diff := cmp.Diff(wantList, list); diff != "" {
		t.Errorf("MaterializedViews (-want +got):\n%s", diff)
	}
	if err := f.iadmin.DeleteMaterializedView(ctx, "i", "v_expired"); err != nil {
		t.Fatalf("DeleteMaterializedView: %v", err)
	}
	list, err = f.iadmin.MaterializedViews(ctx, "i")
	if err != nil {
		t.Fatalf("MaterializedViews: %v", err)
	}
	if diff := cmp.Diff(wantList[1:], list); diff != "" {
		t.Errorf("MaterializedViews after delete (-want +got):\n%s", diff)
	}
}

// totalsViewRows are the totals of totalsRows in the view's key order: the GROUP BY columns in clause order,
// compared as encoded bytes, so "b2" < "default" < $ within t1.
func totalsViewRows() [][]any {
	b := func(s string) []byte { return []byte(s) }
	return [][]any{
		{b("t1"), str(`"b2"`), b("p1"), b("n"), i64(1), i64(0), i64(0)},
		{b("t1"), str(`"default"`), b("p1"), b("n"), i64(2), i64(0), i64(0)},
		{b("t1"), str("$"), b("p1"), b("b"), i64(1), i64(70), i64(1)},
		{b("t1"), str("$"), b("p1"), b("n"), i64(2), i64(150), i64(2)},
		{b("t2"), str("$"), b("p1"), b("n"), i64(1), i64(10), i64(1)},
	}
}

func TestViewReadReturnsQueryResultInKeyOrder(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_totals", totalsQuery)
	f.createView(ctx, t, "v_expired", expiredViewQuery)

	totals := f.query(ctx, t, "SELECT * FROM v_totals", nil, nil)
	expired := f.query(ctx, t, "SELECT rowKey FROM v_expired", nil, nil)

	want := sqlResult{cols: totalsColumns, types: totalsTypes, rows: totalsViewRows()}
	if diff := cmp.Diff(want, totals, cmp.AllowUnexported(sqlResult{})); diff != "" {
		t.Errorf("totals view (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"t1#p1#n#rowB", "t2#p1#n#rowD"}, expired.keys()); diff != "" {
		t.Errorf("expired view keys (-want +got):\n%s", diff)
	}
}

func TestViewCreateRejectsQueriesAsProductionDoes(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)

	for _, tc := range []struct {
		name, id, query string
		code            codes.Code
		msg             string
	}{
		{
			"query fails planning", "v_bad", "SELECT nosuch FROM `items-prod` GROUP BY nosuch",
			codes.InvalidArgument, "Unrecognized name: nosuch [at 1:8]",
		},
		{
			"no GROUP BY or ORDER BY", "v_bad", "SELECT _key AS rk FROM `items-prod` WHERE mark['flag'] IS NULL",
			codes.InvalidArgument, "queries must contain a GROUP BY or ORDER BY clause",
		},
		{
			"ORDER BY DESC", "v_bad", "SELECT _key AS rk FROM `items-prod` ORDER BY rk DESC",
			codes.InvalidArgument, "Only ascending order in ORDER BY by is supported in materialized views.",
		},
		{
			"LIMIT", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `items-prod` GROUP BY t LIMIT 5",
			codes.InvalidArgument, "Limit and offset are not supported in materialized views.",
		},
		{
			"GROUP BY under ORDER BY", "v_bad", "SELECT t, n FROM (SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `items-prod` GROUP BY t) ORDER BY n",
			codes.InvalidArgument, "ORDER BY is not supported with GROUP BY in materialized views.",
		},
		{
			"ORDER BY under GROUP BY", "v_bad", "SELECT t, COUNT(*) AS n FROM (SELECT SPLIT(_key, '#')[0] AS t FROM `items-prod` ORDER BY t) GROUP BY t",
			codes.InvalidArgument, "ORDER BY is not supported with GROUP BY in materialized views.",
		},
		{
			"GROUP BY with ORDER BY", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `items-prod` GROUP BY t ORDER BY t",
			codes.InvalidArgument, "ORDER BY is not supported with GROUP BY in materialized views.",
		},
		{
			"two GROUP BYs", "v_bad", "SELECT n, COUNT(*) AS c FROM (SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `items-prod` GROUP BY t) GROUP BY n",
			codes.InvalidArgument, "This query is not valid. Please ensure that all parts of the query are valid, as per the requirements listed at https://cloud.google.com/bigtable/docs/reference/sql/googlesql-reference-overview. In particular, the query must not use multiple GROUP BY or ORDER BY clauses.",
		},
		{
			"ORDER BY without the key", "v_bad", "SELECT TO_INT64(size['bytes']) AS sz, _key AS rk FROM `items-prod` ORDER BY sz",
			codes.InvalidArgument, "queries must select and order by the unmodified _key column from the source table",
		},
		{
			"ORDER BY key not selected", "v_bad", "SELECT TO_INT64(size['bytes']) AS sz FROM `items-prod` ORDER BY sz, _key",
			codes.InvalidArgument, "every ORDER BY column must be selected in the final query",
		},
		{
			"ORDER BY expression not selected", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, _key AS rk FROM `items-prod` ORDER BY t, SPLIT(_key, '#')[1]",
			codes.InvalidArgument, "every ORDER BY column must be selected in the final query",
		},
		{
			"GROUP BY expression not selected", "v_bad", "SELECT COUNT(*) AS n FROM `items-prod` GROUP BY SPLIT(_key, '#')[0]",
			codes.InvalidArgument, "every GROUP BY column must be selected in the final query",
		},
		{
			"ORDER BY without the selected key", "v_bad", "SELECT TO_INT64(size['bytes']) AS sz, _key FROM `items-prod` ORDER BY sz",
			codes.InvalidArgument, "queries must select and order by the unmodified _key column from the source table",
		},
		{
			"GROUP BY _key and another column", "v_bad", "SELECT _key, TO_INT64(size['bytes']) AS sz FROM `items-prod` GROUP BY _key, sz",
			codes.InvalidArgument, "queries that provide a _key hint must only group by _key (and optionally _timestamp). Use a different column name if you want to create a composite key.",
		},
		{
			"aggregate named _key", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, MAX(_key) AS _key, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "queries that provide a _key hint must only group by _key (and optionally _timestamp). Use a different column name if you want to create a composite key.",
		},
		{
			"ANY_VALUE under a taken ID", "v_expired", "SELECT _key, ANY_VALUE(size['bytes']) AS sz, COUNT(*) AS n FROM `items-prod` GROUP BY _key",
			codes.InvalidArgument, "Only stable functions are supported in materialized views (GoogleSQL:any_value is not stable)",
		},
		{
			"CURRENT_TIMESTAMP", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, CURRENT_TIMESTAMP() AS ts, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:current_timestamp is not immutable)",
		},
		{
			"RAND", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, RAND() AS r, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:rand is not immutable)",
		},
		{
			"GENERATE_UUID", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, GENERATE_UUID() AS u, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:generate_uuid is not immutable)",
		},
		{
			"CURRENT_DATE", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, CURRENT_DATE() AS d, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:current_date is not immutable)",
		},
		{
			"RAND under CAST", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, CAST(RAND() AS STRING) AS r, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:rand is not immutable)",
		},
		{
			"GENERATE_UUID under CONCAT", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, CONCAT(GENERATE_UUID(), 'x') AS u, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:generate_uuid is not immutable)",
		},
		{
			"RAND in WHERE of a GROUP BY view", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `items-prod` WHERE RAND() < 0.5 GROUP BY t",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:rand is not immutable)",
		},
		{
			"RAND in WHERE of an ORDER BY view", "v_bad", "SELECT _key, TO_INT64(size['bytes']) AS sz FROM `items-prod` WHERE RAND() < 0.5 ORDER BY _key",
			codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:rand is not immutable)",
		},
		{
			"ARRAY_AGG", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, ARRAY_AGG(_key) AS ks, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only stable functions are supported in materialized views (GoogleSQL:array_agg is not stable)",
		},
		{
			"STRING_AGG", "v_bad", "SELECT SPLIT(_key, '#')[0] AS t, STRING_AGG(_key) AS ks, COUNT(*) AS n FROM `items-prod` GROUP BY t",
			codes.InvalidArgument, "Only stable functions are supported in materialized views (GoogleSQL:string_agg is not stable)",
		},
		{
			"valid query under a taken ID", "v_expired", expiredViewQuery,
			codes.AlreadyExists, "Materialized View v_expired already exists.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.iadmin.CreateMaterializedView(ctx, "i", &bigtable.MaterializedViewInfo{MaterializedViewID: tc.id, Query: tc.query})
			wantStatus(t, err, tc.code, tc.msg)
		})
	}
	if _, err := f.iadmin.MaterializedViewInfo(ctx, "i", "v_bad"); status.Code(err) != codes.NotFound {
		t.Errorf("MaterializedViewInfo(v_bad) error = %v, want NotFound", err)
	}
}

func TestViewOrdersByAnAliasedKeyAfterAnotherColumn(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_bysize", "SELECT TO_INT64(size['bytes']) AS sz, _key AS rk FROM `items-prod` ORDER BY sz, rk")

	got := f.query(ctx, t, "SELECT * FROM v_bysize", nil, nil)

	b := func(s string) []byte { return []byte(s) }
	want := [][]any{
		{i64(10), b("t2#p1#n#rowD")},
		{i64(50), b("t1#p1#n#rowB")},
		{i64(70), b("t1#p1#b#rowC")},
		{i64(100), b("t1#p1#n#rowA")},
	}
	if diff := cmp.Diff(want, got.rows); diff != "" {
		t.Errorf("rows (-want +got):\n%s", diff)
	}
}

// Production's response to an aggregate without GROUP BY is unmeasured. The emulator applies the rule that a view
// needs a GROUP BY or ORDER BY.
func TestViewCreateRejectsAggregateWithoutGroupBy(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)

	err := f.iadmin.CreateMaterializedView(ctx, "i", &bigtable.MaterializedViewInfo{MaterializedViewID: "v_bad", Query: "SELECT COUNT(*) AS n FROM `items-prod`"})
	wantStatus(t, err, codes.InvalidArgument, "queries must contain a GROUP BY or ORDER BY clause")
}

func TestViewUpdateTogglesOnlyDeletionProtection(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)

	err := f.iadmin.UpdateMaterializedView(ctx, "i", bigtable.MaterializedViewInfo{MaterializedViewID: "v_expired", Query: "SELECT _key AS rk FROM `items-prod` ORDER BY rk"})
	// The Go client wraps the RPC error.
	wantStatus(t, errors.Unwrap(err), codes.InvalidArgument, "Immutable fields 'query,name' cannot be updated.")
	if err := f.iadmin.UpdateMaterializedView(ctx, "i", bigtable.MaterializedViewInfo{MaterializedViewID: "v_expired", Query: expiredViewQuery}); err != nil {
		t.Errorf("update with the stored query: %v", err)
	}
	if err := f.iadmin.UpdateMaterializedView(ctx, "i", bigtable.MaterializedViewInfo{MaterializedViewID: "v_expired", DeletionProtection: bigtable.Protected}); err != nil {
		t.Fatalf("update deletion protection: %v", err)
	}
	got, err := f.iadmin.MaterializedViewInfo(ctx, "i", "v_expired")
	if err != nil {
		t.Fatalf("MaterializedViewInfo: %v", err)
	}
	want := &bigtable.MaterializedViewInfo{MaterializedViewID: "v_expired", Query: expiredViewQuery, DeletionProtection: bigtable.Protected}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("MaterializedViewInfo (-want +got):\n%s", diff)
	}

	err = f.iadmin.DeleteMaterializedView(ctx, "i", "v_expired")
	wantStatus(t, err, codes.FailedPrecondition, "Materialized View projects/p/instances/i/materializedViews/v_expired has deletion protection enabled.")

	if err := f.iadmin.UpdateMaterializedView(ctx, "i", bigtable.MaterializedViewInfo{MaterializedViewID: "v_expired", DeletionProtection: bigtable.Unprotected}); err != nil {
		t.Fatalf("update deletion protection: %v", err)
	}
	if err := f.iadmin.DeleteMaterializedView(ctx, "i", "v_expired"); err != nil {
		t.Errorf("DeleteMaterializedView after unprotecting: %v", err)
	}
}

func TestViewReadTakesLimitAndParameters(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_totals", totalsQuery)

	limited := f.query(ctx, t, "SELECT * FROM v_totals LIMIT 1", nil, nil)
	bound := f.query(ctx, t, "SELECT * FROM v_totals WHERE tenantId = @tenant AND labelId = @label",
		map[string]bigtable.SQLType{"tenant": bigtable.BytesSQLType{}, "label": bigtable.StringSQLType{}},
		map[string]any{"tenant": []byte("t1"), "label": "$"})

	rows := totalsViewRows()
	if diff := cmp.Diff(rows[:1], limited.rows); diff != "" {
		t.Errorf("LIMIT 1 rows (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(rows[2:4], bound.rows); diff != "" {
		t.Errorf("bound rows (-want +got):\n%s", diff)
	}
}

func TestViewLeavesOutSourceRowWhoseEvaluationFails(t *testing.T) {
	ctx := sqlContext(t)
	// t3#bad has two key parts, so SPLIT(_key, '#')[2] is out of range.
	f := newViewFixture(ctx, t, append(slices.Clone(totalsRows), itemRow{key: "t3#bad", size: i64(1)}))
	f.createView(ctx, t, "v_totals", totalsQuery)

	got := f.query(ctx, t, "SELECT * FROM v_totals", nil, nil)
	_, err := f.executeErr(ctx, t, totalsQuery)

	if diff := cmp.Diff(totalsViewRows(), got.rows); diff != "" {
		t.Errorf("totals view rows (-want +got):\n%s", diff)
	}
	wantStatus(t, err, codes.OutOfRange, "Array index 2 is out of bounds")
}

func TestViewLeavesOutGroupWhoseAggregateFails(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, runtimeErrorRows)
	const sumQuery = "SELECT SPLIT(_key, '#')[0] AS t, SUM(TO_INT64(size['bytes'])) AS s, COUNT(*) AS n FROM `items-prod` WHERE STARTS_WITH(_key, 't') GROUP BY t"
	f.createView(ctx, t, "v_sum", sumQuery)

	got := f.query(ctx, t, "SELECT * FROM v_sum", nil, nil)
	_, err := f.executeErr(ctx, t, sumQuery)

	b := func(s string) []byte { return []byte(s) }
	want := [][]any{
		{b("t1"), i64(220), i64(3)},
		{b("t6"), i64(7), i64(1)},
		{b("t7"), (*int64)(nil), i64(1)},
	}
	if diff := cmp.Diff(want, got.rows); diff != "" {
		t.Errorf("SUM view rows (-want +got):\n%s", diff)
	}
	wantStatus(t, err, codes.OutOfRange, "SUM() aggregation overflow")
}

func TestViewLeavesOutRowWhoseAggregateArgumentFails(t *testing.T) {
	ctx := sqlContext(t)
	// TO_INT64 fails on a 3-byte labels cell after the row's group key evaluates. The row then adds no group and no count.
	f := newViewFixture(ctx, t, []itemRow{
		{key: "t1#a", labels: str(string(be(5)))},
		{key: "t1#b", labels: str("abc")},
		{key: "t2#c", labels: str("abc")},
	})
	f.createView(ctx, t, "v_sum", "SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n, SUM(TO_INT64(labels['labels'])) AS s FROM `items-prod` GROUP BY t")

	got := f.query(ctx, t, "SELECT * FROM v_sum", nil, nil)

	want := [][]any{{[]byte("t1"), i64(1), i64(5)}}
	if diff := cmp.Diff(want, got.rows); diff != "" {
		t.Errorf("view rows (-want +got):\n%s", diff)
	}
}

// Production's view over the SQL fixture on 2026-10-19 held no rows: each t1 row unnests a 1 that passes, then a 0
// that fails, and production left the whole source row out.
func TestViewLeavesOutWholeSourceRowWhenALaterUnnestedRowFails(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, []itemRow{{key: "t1#a", size: i64(1)}, {key: "t1#b", size: i64(1)}, {key: "t2#c", size: i64(1)}})
	const unnest = "SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `items-prod`, UNNEST(CASE WHEN STARTS_WITH(_key, 't1') THEN [1, 0] ELSE %s END) AS d WHERE DIV(1, d) > 0 GROUP BY t"
	f.createView(ctx, t, "v_allfail", fmt.Sprintf(unnest, "[0, 1]"))
	f.createView(ctx, t, "v_t2passes", fmt.Sprintf(unnest, "[1]"))

	allFailRead := f.readView(ctx, t, "v_allfail")
	allFailSQL := f.query(ctx, t, "SELECT * FROM v_allfail", nil, nil)
	t2Passes := f.readView(ctx, t, "v_t2passes")

	if len(allFailRead) != 0 || len(allFailSQL.rows) != 0 {
		t.Errorf("view whose every source row fails: ReadRows = %v, SQL rows = %v, want none", allFailRead, allFailSQL.rows)
	}
	want := []bigtable.Row{viewRow("t2", bigtable.ReadItem{Column: "n", Value: be(1)})}
	if diff := cmp.Diff(want, t2Passes); diff != "" {
		t.Errorf("ReadRows on the view whose t2 row passes (-want +got):\n%s", diff)
	}
}

// labelRows are the SQL fixture's rows that have labels. rowA's two labels give it two view rows under one key in a
// view keyed by _key alone.
var labelRows = []itemRow{
	{key: "t1#p1#n#rowA", labels: str(`["default","b2"]`)},
	{key: "t1#p1#n#rowB", labels: str(`["default"]`)},
	{key: "t2#p1#n#rowD", labels: str(`[]`)},
	{key: "t3#p1#m#rowE", labels: str(`["a\"b"]`)},
	{key: "t3#p1#n#rowF", labels: str(`["default"]`)},
}

// Production's view over the SQL fixture on 2026-10-19 left out rowA, whose two labels share the key, on ReadRows and
// SQL alike.
func TestViewLeavesOutSourceRowWhoseUnnestedRowsShareAKey(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, labelRows)
	f.createView(ctx, t, "v_bylabel", "SELECT _key, l FROM `items-prod`, UNNEST(JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING))) AS l ORDER BY _key")

	read := f.readView(ctx, t, "v_bylabel")
	got := f.query(ctx, t, "SELECT * FROM v_bylabel", nil, nil)

	wantRead := []bigtable.Row{
		viewRow("t1#p1#n#rowB", bigtable.ReadItem{Column: "l", Value: []byte(`"default"`)}),
		viewRow("t3#p1#m#rowE", bigtable.ReadItem{Column: "l", Value: []byte(`"a\"b"`)}),
		viewRow("t3#p1#n#rowF", bigtable.ReadItem{Column: "l", Value: []byte(`"default"`)}),
	}
	if diff := cmp.Diff(wantRead, read); diff != "" {
		t.Errorf("ReadRows on the label view (-want +got):\n%s", diff)
	}
	wantSQL := [][]any{
		{[]byte("t1#p1#n#rowB"), str(`"default"`)},
		{[]byte("t3#p1#m#rowE"), str(`"a\"b"`)},
		{[]byte("t3#p1#n#rowF"), str(`"default"`)},
	}
	if diff := cmp.Diff(wantSQL, got.rows); diff != "" {
		t.Errorf("SELECT * on the label view (-want +got):\n%s", diff)
	}
}

// In a view keyed by _key and the label, only rowG's repeated label shares a key, and the whole of rowG is left out.
// rowA's two distinct labels keep both its rows.
func TestViewLeavesOutWholeSourceRowWhenTwoOfItsRowsShareAKey(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, append(slices.Clone(labelRows), itemRow{key: "t4#rowG", labels: str(`["x","y","x"]`)}))
	f.createView(ctx, t, "v_bykeylabel", "SELECT _key, l FROM `items-prod`, UNNEST(JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING))) AS l ORDER BY _key, l")

	got := f.query(ctx, t, "SELECT * FROM v_bykeylabel", nil, nil)

	want := [][]any{
		{[]byte("t1#p1#n#rowA"), str(`"b2"`)},
		{[]byte("t1#p1#n#rowA"), str(`"default"`)},
		{[]byte("t1#p1#n#rowB"), str(`"default"`)},
		{[]byte("t3#p1#m#rowE"), str(`"a\"b"`)},
		{[]byte("t3#p1#n#rowF"), str(`"default"`)},
	}
	if diff := cmp.Diff(want, got.rows); diff != "" {
		t.Errorf("SELECT * on the key and label view (-want +got):\n%s", diff)
	}
}

func TestViewExposesKeyOnlyAsAnOutputColumn(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)
	f.createView(ctx, t, "v_bykey", "SELECT _key FROM `items-prod` WHERE mark['flag'] IS NULL GROUP BY _key")

	_, err := f.client.PrepareStatement(ctx, "SELECT _key FROM v_expired", nil)
	got := f.query(ctx, t, "SELECT * FROM v_bykey", nil, nil)

	wantStatus(t, err, codes.InvalidArgument, "Unrecognized name: _key [at 1:8]")
	want := sqlResult{
		cols:  []string{"_key"},
		types: []bigtable.SQLType{bigtable.BytesSQLType{}},
		rows:  [][]any{{[]byte("t1#p1#n#rowB")}, {[]byte("t2#p1#n#rowD")}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(sqlResult{})); diff != "" {
		t.Errorf("_key view (-want +got):\n%s", diff)
	}
}

func TestViewKeepsGroupWithNullKeyFirst(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, runtimeErrorRows)
	f.createView(ctx, t, "v_sizes", "SELECT TO_INT64(size['bytes']) AS sz, COUNT(*) AS n FROM `items-prod` WHERE STARTS_WITH(_key, 't5') OR STARTS_WITH(_key, 't7') GROUP BY sz")

	got := f.query(ctx, t, "SELECT * FROM v_sizes", nil, nil)

	want := [][]any{
		{(*int64)(nil), i64(1)},
		{i64(1), i64(1)},
		{i64(9223372036854775807), i64(1)},
	}
	if diff := cmp.Diff(want, got.rows); diff != "" {
		t.Errorf("rows (-want +got):\n%s", diff)
	}
}

func TestDeleteTableFailsWhileAViewReadsIt(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)

	err := f.admin.DeleteTable(ctx, "items-prod")

	wantStatus(t, err, codes.FailedPrecondition, "Unable to delete resource projects/p/instances/i/tables/items-prod because the resource is referenced by another resource. The existing references are: {projects/p/instances/i/materializedViews/v_expired}")
	if err := f.iadmin.DeleteMaterializedView(ctx, "i", "v_expired"); err != nil {
		t.Fatalf("DeleteMaterializedView: %v", err)
	}
	if err := f.admin.DeleteTable(ctx, "items-prod"); err != nil {
		t.Errorf("DeleteTable after the view is gone: %v", err)
	}
}

// A view created while DeleteTable runs on its table either fails or keeps the table, so no view reads a deleted
// table.
func TestViewCreateRacingDeleteTableLeavesNoViewOnADeletedTable(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, nil)
	for i := range 20 {
		table := fmt.Sprintf("race-%d", i)
		f.createItemsTable(ctx, t, table, nil)
		var createErr, deleteErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			createErr = f.iadmin.CreateMaterializedView(ctx, "i", &bigtable.MaterializedViewInfo{
				MaterializedViewID: fmt.Sprintf("v%d", i),
				Query:              "SELECT _key AS rowKey FROM `" + table + "` ORDER BY rowKey",
			})
		})
		wg.Go(func() { deleteErr = f.admin.DeleteTable(ctx, table) })
		wg.Wait()
		if createErr == nil && deleteErr == nil {
			t.Fatalf("CreateMaterializedView and DeleteTable on %s both succeeded, so view v%d reads a deleted table", table, i)
		}
	}
}

func TestViewExecuteFailsOnceTheViewIsDeleted(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)
	ps, err := f.client.PrepareStatement(ctx, "SELECT rowKey FROM v_expired", nil)
	if err != nil {
		t.Fatalf("PrepareStatement: %v", err)
	}
	if err := f.iadmin.DeleteMaterializedView(ctx, "i", "v_expired"); err != nil {
		t.Fatalf("DeleteMaterializedView: %v", err)
	}

	bs, err := ps.Bind(nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	err = bs.Execute(ctx, func(bigtable.ResultRow) bool { return true })

	// Production names the project by number and the view's replica by cluster. The emulator has the project ID and
	// no cluster, so it writes the ID in the number's slot and names its one cluster after the instance.
	wantStatus(t, err, codes.NotFound, "Failed to read: projects/{p}/instances/i/clusters/i-c1/materializedViews/v_expired : MaterializedViewsReplicas(p,i,i-c1,v_expired) : Failed to read: projects/{p}/instances/i/clusters/i-c1/materializedViews/v_expired")
}

func TestViewExecuteUnderBareInstanceFailsOnceTheViewIsDeleted(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	admin := btapb.NewBigtableTableAdminClient(f.conn)
	_, err := admin.CreateTable(ctx, &btapb.CreateTableRequest{
		Parent:  "i",
		TableId: "t",
		Table:   &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"f": {}}},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	iadmin := btapb.NewBigtableInstanceAdminClient(f.conn)
	_, err = iadmin.CreateMaterializedView(ctx, &btapb.CreateMaterializedViewRequest{
		Parent:             "i",
		MaterializedViewId: "v",
		MaterializedView:   &btapb.MaterializedView{Query: "SELECT _key AS rowKey FROM `t` ORDER BY rowKey"},
	})
	if err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	data := btpb.NewBigtableClient(f.conn)
	prepared, err := data.PrepareQuery(ctx, &btpb.PrepareQueryRequest{InstanceName: "i", Query: "SELECT rowKey FROM v"})
	if err != nil {
		t.Fatalf("PrepareQuery: %v", err)
	}
	if _, err := iadmin.DeleteMaterializedView(ctx, &btapb.DeleteMaterializedViewRequest{Name: "i/materializedViews/v"}); err != nil {
		t.Fatalf("DeleteMaterializedView: %v", err)
	}

	_, err = f.executeOn(ctx, t, "i", prepared.PreparedQuery)
	wantStatus(t, err, codes.NotFound, "Failed to read: i/materializedViews/v")

	if _, err := admin.GetTable(ctx, &btapb.GetTableRequest{Name: "i/tables/t"}); err != nil {
		t.Errorf("GetTable after the failed query: %v", err)
	}
}

// Production never ran a query prepared on a view after the view was deleted and recreated, as a probe of
// production recorded on 2026-10-19.
func TestViewExecuteExpiresOnceTheViewIsRecreated(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v", expiredViewQuery)
	f.createView(ctx, t, "v_kept", expiredViewQuery)
	old := f.prepareRaw(ctx, t, "SELECT rowKey FROM v")
	kept := f.prepareRaw(ctx, t, "SELECT rowKey FROM v_kept")

	if err := f.iadmin.DeleteMaterializedView(ctx, "i", "v"); err != nil {
		t.Fatalf("DeleteMaterializedView: %v", err)
	}
	f.createView(ctx, t, "v", "SELECT SPLIT(_key, '#')[0] AS tenant, COUNT(*) AS n FROM `items-prod` GROUP BY tenant")

	msgs, err := f.executeRaw(ctx, t, old)
	checkExpired(t, msgs, err, expiredQuery)
	if _, err := f.executeRaw(ctx, t, kept); err != nil {
		t.Errorf("ExecuteQuery on the untouched view: %v", err)
	}
	got := f.query(ctx, t, "SELECT tenant, n FROM v ORDER BY tenant", nil, nil)
	want := sqlResult{
		cols:  []string{"tenant", "n"},
		types: []bigtable.SQLType{bigtable.BytesSQLType{}, bigtable.Int64SQLType{}},
		rows:  [][]any{{[]byte("t1"), i64(3)}, {[]byte("t2"), i64(1)}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(sqlResult{})); diff != "" {
		t.Errorf("a fresh query on the recreated view (-want +got):\n%s", diff)
	}
}

func (f *viewFixture) readView(ctx context.Context, t *testing.T, id string) []bigtable.Row {
	t.Helper()
	var rows []bigtable.Row
	err := f.client.OpenMaterializedView(id).ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		rows = append(rows, r)
		return true
	})
	if err != nil {
		t.Fatalf("ReadRows(%s): %v", id, err)
	}
	return rows
}

// viewRow is a view row as production's ReadRows returns it: one family, default, with an empty-qualifier cell
// with no value, then a cell per stored column in qualifier order, all at timestamp 0.
func viewRow(key string, cells ...bigtable.ReadItem) bigtable.Row {
	items := []bigtable.ReadItem{{Row: key, Column: "default:"}}
	for _, c := range cells {
		c.Row, c.Column = key, "default:"+c.Column
		items = append(items, c)
	}
	return bigtable.Row{"default": items}
}

func be(n int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(n)) }

// Production stores a GROUP BY view's group columns in the key only, with the parts joined by \x00\x01, and its
// aggregates as cells, an INT64 as 8 big-endian bytes.
func TestViewReadRowsReturnsGroupedRowsAsCells(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_totals", totalsQuery)

	got := f.readView(ctx, t, "v_totals")

	cells := func(items, bytes, groupItems int64) []bigtable.ReadItem {
		return []bigtable.ReadItem{
			{Column: "itemCount", Value: be(items)},
			{Column: "tenantPartitionType_bytes", Value: be(bytes)},
			{Column: "tenantPartitionType_itemCount", Value: be(groupItems)},
		}
	}
	want := []bigtable.Row{
		viewRow("t1\x00\x01\"b2\"\x00\x01p1\x00\x01n", cells(1, 0, 0)...),
		viewRow("t1\x00\x01\"default\"\x00\x01p1\x00\x01n", cells(2, 0, 0)...),
		viewRow("t1\x00\x01$\x00\x01p1\x00\x01b", cells(1, 70, 1)...),
		viewRow("t1\x00\x01$\x00\x01p1\x00\x01n", cells(2, 150, 2)...),
		viewRow("t2\x00\x01$\x00\x01p1\x00\x01n", cells(1, 10, 1)...),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadRows on the totals view (-want +got):\n%s", diff)
	}
}

// An ORDER BY view keyed by an alias of _key keeps the source key as its row key and stores the column as a cell
// too.
func TestViewReadRowsReturnsOrderedRowsWithTheirKeyColumn(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)

	got := f.readView(ctx, t, "v_expired")

	want := []bigtable.Row{
		viewRow("t1#p1#n#rowB", bigtable.ReadItem{Column: "rowKey", Value: []byte("t1#p1#n#rowB")}),
		viewRow("t2#p1#n#rowD", bigtable.ReadItem{Column: "rowKey", Value: []byte("t2#p1#n#rowD")}),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadRows on the expired view (-want +got):\n%s", diff)
	}
}

// A NULL aggregate has no cell. A view keyed by the _key column alone keeps the raw key and stores no cell for it.
func TestViewReadRowsLeavesOutNullCellsAndTheLoneKeyColumn(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, runtimeErrorRows)
	f.createView(ctx, t, "v_sum", "SELECT SPLIT(_key, '#')[0] AS t, SUM(TO_INT64(size['bytes'])) AS s, COUNT(*) AS n FROM `items-prod` WHERE STARTS_WITH(_key, 't') GROUP BY t")
	f.createView(ctx, t, "v_bykey", "SELECT _key, TO_INT64(size['bytes']) AS sz FROM `items-prod` WHERE STARTS_WITH(_key, 't6') ORDER BY _key")

	sum := f.readView(ctx, t, "v_sum")
	byKey := f.readView(ctx, t, "v_bykey")

	wantSum := []bigtable.Row{
		viewRow("t1", bigtable.ReadItem{Column: "n", Value: be(3)}, bigtable.ReadItem{Column: "s", Value: be(220)}),
		viewRow("t6", bigtable.ReadItem{Column: "n", Value: be(1)}, bigtable.ReadItem{Column: "s", Value: be(7)}),
		viewRow("t7", bigtable.ReadItem{Column: "n", Value: be(1)}),
	}
	if diff := cmp.Diff(wantSum, sum); diff != "" {
		t.Errorf("ReadRows on the SUM view (-want +got):\n%s", diff)
	}
	wantByKey := []bigtable.Row{viewRow("t6#p1#n#rowI", bigtable.ReadItem{Column: "sz", Value: be(7)})}
	if diff := cmp.Diff(wantByKey, byKey); diff != "" {
		t.Errorf("ReadRows on the _key view (-want +got):\n%s", diff)
	}
}

// Production takes the _key column unencoded only when it is the whole key. In a multi-part key it escapes the part
// as it does any BYTES part, and stores every column as a cell.
func TestViewReadRowsEscapesTheKeyColumnInsideAMultiPartKey(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, []itemRow{{key: "k", size: i64(3)}, {key: "k\x00\x00", size: i64(2)}})
	f.createView(ctx, t, "v_bykeysize", "SELECT _key, TO_INT64(size['bytes']) AS sz FROM `items-prod` ORDER BY _key, sz")

	got := f.readView(ctx, t, "v_bykeysize")

	want := []bigtable.Row{
		viewRow("k\x00\x01\x83", bigtable.ReadItem{Column: "_key", Value: []byte("k")}, bigtable.ReadItem{Column: "sz", Value: be(3)}),
		viewRow("k\x00\xff\x00\xff\x00\x01\x82", bigtable.ReadItem{Column: "_key", Value: []byte("k\x00\x00")}, bigtable.ReadItem{Column: "sz", Value: be(2)}),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadRows on the multi-part _key view (-want +got):\n%s", diff)
	}
}

func BenchmarkViewReadRow(b *testing.B) {
	ctx := context.Background()
	f := newViewFixture(ctx, b, totalsRows)
	f.createView(ctx, b, "v_expired", expiredViewQuery)
	v := f.client.OpenMaterializedView("v_expired")
	for b.Loop() {
		r, err := v.ReadRow(ctx, "t1#p1#n#rowB")
		if err != nil {
			b.Fatal(err)
		}
		if r.Key() != "t1#p1#n#rowB" {
			b.Fatalf("ReadRow = %v, want row t1#p1#n#rowB", r)
		}
	}
}

func BenchmarkPrepareQueryOverView(b *testing.B) {
	ctx := context.Background()
	f := newViewFixture(ctx, b, totalsRows)
	f.createView(ctx, b, "v_expired", expiredViewQuery)
	for b.Loop() {
		if _, err := f.client.PrepareStatement(ctx, "SELECT rowKey FROM v_expired", nil); err != nil {
			b.Fatal(err)
		}
	}
}

// A drop of a family a view reads fails with production's message, and the family and the view stay as they were.
// A drop of a family no view reads succeeds.
func TestDropFamilyFailsWhileAViewReadsIt(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_labels", "SELECT _key, l FROM `items-prod`, UNNEST(JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING))) AS l ORDER BY _key")
	before := f.readView(ctx, t, "v_labels")
	if len(before) == 0 {
		t.Fatal("the view has no rows before the drop")
	}

	err := f.admin.DeleteColumnFamily(ctx, "items-prod", "labels")

	wantStatus(t, err, codes.FailedPrecondition, "Unable to delete the column family: projects/p/instances/i/tables/items-prod/columnFamilies/labels because it is currently referenced by the following resources: {projects/p/instances/i/materializedViews/v_labels}.")
	info, err := f.admin.TableInfo(ctx, "items-prod")
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if !slices.Contains(info.Families, "labels") {
		t.Errorf("families after the refused drop = %v, want labels among them", info.Families)
	}
	if diff := cmp.Diff(before, f.readView(ctx, t, "v_labels")); diff != "" {
		t.Errorf("view rows after the refused drop (-before +after):\n%s", diff)
	}
	if err := f.admin.DeleteColumnFamily(ctx, "items-prod", "size"); err != nil {
		t.Errorf("DeleteColumnFamily(size), which no view reads: %v", err)
	}
}

// Concurrent ReadRows and queries on one view each return the view's rows.
func TestViewServesConcurrentReadsAndQueries(t *testing.T) {
	ctx := sqlContext(t)
	f := newViewFixture(ctx, t, totalsRows)
	f.createView(ctx, t, "v_expired", expiredViewQuery)
	wantKeys := []string{"t1#p1#n#rowB", "t2#p1#n#rowD"}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			var keys []string
			err := f.client.OpenMaterializedView("v_expired").ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
				keys = append(keys, r.Key())
				return true
			})
			if err != nil {
				t.Errorf("ReadRows: %v", err)
			}
			if diff := cmp.Diff(wantKeys, keys); diff != "" {
				t.Errorf("ReadRows keys (-want +got):\n%s", diff)
			}
		})
		wg.Go(func() {
			ps, err := f.client.PrepareStatement(ctx, "SELECT rowKey FROM v_expired", nil)
			if err != nil {
				t.Errorf("PrepareStatement: %v", err)
				return
			}
			bs, err := ps.Bind(nil)
			if err != nil {
				t.Errorf("Bind: %v", err)
				return
			}
			var keys []string
			err = bs.Execute(ctx, func(r bigtable.ResultRow) bool {
				var k []byte
				if err := r.GetByName("rowKey", &k); err != nil {
					t.Errorf("GetByName: %v", err)
				}
				keys = append(keys, string(k))
				return true
			})
			if err != nil {
				t.Errorf("Execute: %v", err)
			}
			if diff := cmp.Diff(wantKeys, keys); diff != "" {
				t.Errorf("query keys (-want +got):\n%s", diff)
			}
		})
	}
	wg.Wait()
}
