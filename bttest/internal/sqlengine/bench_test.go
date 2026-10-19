// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package sqlengine

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
)

// rowSource answers every scan with all of its rows.
type rowSource []Row

func (s rowSource) Scan(_ context.Context, _ ScanRequest, fn func(Row) error) error {
	for _, r := range s {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// benchTotalsQuery is the downstream totals view query as written.
const benchTotalsQuery = `SELECT tenantId, labelId, partitionId, rowType,
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

// BenchmarkTotalsQuery runs the totals query over 1000 source rows that each unnest into 4 rows.
func BenchmarkTotalsQuery(b *testing.B) {
	tables := []Table{{Name: "items-prod", Families: []Family{{Name: "size"}, {Name: "labels"}, {Name: "mark"}}}}
	q, err := Prepare(benchTotalsQuery, tables, nil)
	if err != nil {
		b.Fatal(err)
	}
	const sourceRows, perRow = 1000, 4
	src := make(rowSource, sourceRows)
	for i := range src {
		src[i] = Row{
			Key: fmt.Appendf(nil, "t%d#p%d#n#row%d", i%10, i%3, i),
			Cells: map[string]map[string][]byte{
				"size":   {"bytes": binary.BigEndian.AppendUint64(nil, uint64(i))},
				"labels": {"labels": fmt.Appendf(nil, `["default","l%d","m%d"]`, i%5, i%7)},
			},
		}
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		var counted int64
		if err := q.Run(ctx, src, nil, func(row []Value) error { counted += row[4].Int; return nil }); err != nil {
			b.Fatal(err)
		}
		if counted != sourceRows*perRow {
			b.Fatalf("the groups count %d unnested rows, want %d", counted, sourceRows*perRow)
		}
	}
	b.ReportMetric(sourceRows*perRow, "unnested-rows/op")
}
