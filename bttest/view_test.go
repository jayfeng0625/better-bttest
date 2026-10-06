// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"cloud.google.com/go/bigtable"
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

func newViewFixture(ctx context.Context, t *testing.T, rows []itemRow) *viewFixture {
	t.Helper()
	f := newSQLFixture(ctx, t)
	iadmin, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(f.conn))
	if err != nil {
		t.Fatal(err)
	}
	f.createItemsTable(ctx, t, "items-prod", rows)
	return &viewFixture{sqlFixture: f, iadmin: iadmin}
}

func (f *viewFixture) createView(ctx context.Context, t *testing.T, id, query string) {
	t.Helper()
	if err := f.iadmin.CreateMaterializedView(ctx, "i", &bigtable.MaterializedViewInfo{MaterializedViewID: id, Query: query}); err != nil {
		t.Fatalf("CreateMaterializedView(%s): %v", id, err)
	}
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
		{key: "t1#a", labels: str(string(binary.BigEndian.AppendUint64(nil, 5)))},
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

// readView reads every row of the view through the Go client, as production's ReadRows returns them.
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
