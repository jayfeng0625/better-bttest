// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"cloud.google.com/go/bigtable"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
)

// itemRow is one seeded row of an items table. A nil field leaves its cell unset.
type itemRow struct {
	key    string
	size   *uint64
	labels *string
	flag   bool
	// flagDeleted writes the flag cell, then deletes it.
	flagDeleted bool
}

func u64(v uint64) *uint64 { return &v }
func str(v string) *string { return &v }

// createItemsTable creates a table with families size, labels, and mark, and writes the rows. A size is 8
// big-endian bytes, as the Node client writes a number.
func (f *sqlFixture) createItemsTable(ctx context.Context, t *testing.T, name string, rows []itemRow) {
	t.Helper()
	if err := f.admin.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: name, ColumnFamilies: map[string]bigtable.Family{
		"size":   {GCPolicy: bigtable.NoGcPolicy()},
		"labels": {GCPolicy: bigtable.NoGcPolicy()},
		"mark":   {GCPolicy: bigtable.NoGcPolicy()},
	}}); err != nil {
		t.Fatal(err)
	}
	tbl := f.client.Open(name)
	for _, r := range rows {
		m := bigtable.NewMutation()
		if r.size != nil {
			m.Set("size", "bytes", 0, binary.BigEndian.AppendUint64(nil, *r.size))
		}
		if r.labels != nil {
			m.Set("labels", "labels", 0, []byte(*r.labels))
		}
		if r.flag || r.flagDeleted {
			m.Set("mark", "flag", 0, []byte("1"))
		}
		if err := tbl.Apply(ctx, r.key, m); err != nil {
			t.Fatal(err)
		}
		if r.flagDeleted {
			del := bigtable.NewMutation()
			del.DeleteCellsInColumn("mark", "flag")
			if err := tbl.Apply(ctx, r.key, del); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// totalsQuery is the downstream totals view query as written, trailing commas and unquoted dashed table included.
const totalsQuery = `SELECT tenantId, labelId, partitionId, rowType,
  COUNT(*) AS itemCount,
  SUM(CASE WHEN '$' = labelId THEN bytes ELSE 0 END) AS tenantPartitionType_bytes,
  SUM(CASE WHEN '$' = labelId THEN 1 ELSE 0 END) AS tenantPartitionType_itemCount,
FROM (SELECT
  SPLIT(_key, '#')[0] AS tenantId,
  SPLIT(_key, '#')[1] AS partitionId,
  SPLIT(_key, '#')[2] AS rowType,
  TO_INT64(size['bytes']) AS bytes,
  ARRAY_CONCAT(['$'], COALESCE(JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING)), [])) AS labels,
  FROM items-prod
) AS expand,
UNNEST(expand.labels) AS labelId
GROUP BY tenantId, labelId, partitionId, rowType`

func TestSQLTotalsQueryCountsRowsPerLabel(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.createItemsTable(ctx, t, "items-prod", []itemRow{
		{key: "t1#p1#n#rowA", size: u64(100), labels: str(`["default","b2"]`), flag: true},
		{key: "t1#p1#n#rowB", size: u64(50), labels: str(`["default"]`)},
		{key: "t1#p1#b#rowC", size: u64(70), flag: true},
		{key: "t2#p1#n#rowD", size: u64(10), labels: str(`[]`), flagDeleted: true},
	})

	got := f.query(ctx, t, totalsQuery+"\nORDER BY tenantId, partitionId, rowType, labelId", nil, nil)

	b := func(s string) []byte { return []byte(s) }
	want := sqlResult{
		cols: []string{"tenantId", "labelId", "partitionId", "rowType", "itemCount", "tenantPartitionType_bytes", "tenantPartitionType_itemCount"},
		types: []bigtable.SQLType{
			bigtable.BytesSQLType{}, bigtable.StringSQLType{}, bigtable.BytesSQLType{}, bigtable.BytesSQLType{},
			bigtable.Int64SQLType{}, bigtable.Int64SQLType{}, bigtable.Int64SQLType{},
		},
		rows: [][]any{
			{b("t1"), str("$"), b("p1"), b("b"), i64(1), i64(70), i64(1)},
			{b("t1"), str(`"b2"`), b("p1"), b("n"), i64(1), i64(0), i64(0)},
			{b("t1"), str(`"default"`), b("p1"), b("n"), i64(2), i64(0), i64(0)},
			{b("t1"), str("$"), b("p1"), b("n"), i64(2), i64(150), i64(2)},
			{b("t2"), str("$"), b("p1"), b("n"), i64(1), i64(10), i64(1)},
		},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(sqlResult{})); diff != "" {
		t.Errorf("result (-want +got):\n%s", diff)
	}
}

// runtimeErrorRows are the rows production held for its runtime error probes on 2026-10-03.
var runtimeErrorRows = []itemRow{
	{key: "t1#p1#n#rowA", size: u64(100), labels: str(`["default","b2"]`), flag: true},
	{key: "t1#p1#n#rowB", size: u64(50), labels: str(`["default"]`)},
	{key: "t1#p1#b#rowC", size: u64(70), flag: true},
	{key: "t5#p1#n#rowG", size: u64(9223372036854775807)},
	{key: "t5#p1#n#rowH", size: u64(1)},
	{key: "t6#p1#n#rowI", size: u64(7)},
	{key: "t7#p1#n#rowJ", labels: str("abcdefgh")},
}

func TestSQLRuntimeErrorFailsExecuteAsProductionDoes(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.createItemsTable(ctx, t, "T", runtimeErrorRows)

	for _, tc := range []struct {
		name, sql string
		code      codes.Code
		msg       string
	}{
		{
			"DIV by zero above GROUP BY",
			"SELECT SPLIT(_key, '#')[0] AS t, DIV(SUM(TO_INT64(size['bytes'])), COUNT(*) - 1) AS q, COUNT(*) AS n FROM T WHERE STARTS_WITH(_key, 't1') OR STARTS_WITH(_key, 't6') GROUP BY t",
			codes.OutOfRange, "division by zero: DIV",
		},
		{
			"SUM overflow",
			"SELECT SPLIT(_key, '#')[0] AS t, SUM(TO_INT64(size['bytes'])) AS s, COUNT(*) AS n FROM T WHERE STARTS_WITH(_key, 't') GROUP BY t",
			codes.OutOfRange, "SUM() aggregation overflow",
		},
		{
			"TO_INT64 on 11 bytes",
			"SELECT SPLIT(_key, '#')[0] AS t, TO_INT64(MAX(labels['labels'])) AS x, COUNT(*) AS n FROM T WHERE STARTS_WITH(_key, 't') GROUP BY t",
			codes.InvalidArgument, "incorrect value size. expected: 8 bytes, actual: 11 bytes",
		},
		{
			"DIV by zero in HAVING",
			"SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM T WHERE STARTS_WITH(_key, 't1') OR STARTS_WITH(_key, 't6') GROUP BY t HAVING DIV(COUNT(*), COUNT(*) - 1) > 0",
			codes.OutOfRange, "division by zero: DIV",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := f.client.PrepareStatement(ctx, tc.sql, nil)
			if err != nil {
				t.Fatalf("PrepareStatement: %v", err)
			}
			bs, err := ps.Bind(nil)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			rows := 0
			err = bs.Execute(ctx, func(bigtable.ResultRow) bool { rows++; return true })
			wantStatus(t, err, tc.code, tc.msg)
			if rows != 0 {
				t.Errorf("got %d rows before the error, want none", rows)
			}
		})
	}
}

func TestSQLGroupByArrayFormsOneGroupPerDistinctArray(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.createItemsTable(ctx, t, "J", []itemRow{
		{key: "rowA", labels: str(`["default","b2"]`)},
		{key: "rowB", labels: str(`["default"]`)},
		{key: "rowC", size: u64(1)},
		{key: "rowD", labels: str(`[]`)},
		{key: "rowE", labels: str(`["default"]`)},
		{key: "rowF", labels: str(`["a\"b"]`)},
	})
	f.createItemsTable(ctx, t, "S", []itemRow{
		{key: "rowA", labels: str("x,y")},
		{key: "rowB", labels: str("x")},
		{key: "rowC", labels: str("x,y")},
		{key: "rowD", labels: str("y,x")},
	})

	for _, tc := range []struct {
		name, sql string
		want      []string
	}{
		{
			"JSON_QUERY_ARRAY",
			"SELECT JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING)) AS a, COUNT(*) AS n FROM J GROUP BY a",
			[]string{`["\"a\\\"b\""] 1`, `["\"default\"" "\"b2\""] 1`, `["\"default\""] 2`, `[] 1`, `null 1`},
		},
		{
			"SPLIT",
			"SELECT SPLIT(labels['labels'], b',') AS a, COUNT(*) AS n FROM S GROUP BY a",
			[]string{`["x" "y"] 2`, `["x"] 1`, `["y" "x"] 1`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.query(ctx, t, tc.sql, nil, nil)
			var groups []string
			for _, row := range got.rows {
				groups = append(groups, fmt.Sprintf("%s %d", groupString(row[0]), *row[1].(*int64)))
			}
			slices.Sort(groups)
			if diff := cmp.Diff(tc.want, groups); diff != "" {
				t.Errorf("groups (-want +got):\n%s", diff)
			}
		})
	}
}

func groupString(v any) string {
	switch a := v.(type) {
	case nil:
		return "null"
	case []*string:
		var elems []string
		for _, e := range a {
			elems = append(elems, *e)
		}
		return fmt.Sprintf("%q", elems)
	}
	return fmt.Sprintf("%q", v)
}
