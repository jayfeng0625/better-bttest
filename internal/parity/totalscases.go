// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"fmt"
	"math"
	"slices"
)

// totalsQuery is the totals view's query as written, trailing commas included, on the case's table.
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
  FROM ` + "`{table}`" + `
) AS expand,
UNNEST(expand.labels) AS labelId
GROUP BY tenantId, labelId, partitionId, rowType`

// The sum# groups each hold three sizes whose total fits INT64, in every order. g7 is a control whose total does not.
var sumGroups = [][]int64{
	{math.MaxInt64, 1, -5},
	{math.MaxInt64, -5, 1},
	{1, math.MaxInt64, -5},
	{1, -5, math.MaxInt64},
	{-5, math.MaxInt64, 1},
	{-5, 1, math.MaxInt64},
	{math.MaxInt64, 1},
}

// The SQL fixture, and the rows sum#g<n>#<a, b, c> with sumGroups' sizes in key order.
func sumFixture() []Call {
	var entries []Entry
	for i, sizes := range sumGroups {
		for j, n := range sizes {
			entries = append(entries, Entry{Row: Row(fmt.Sprintf("sum#g%d#%c", i+1, 'a'+j)), Mutations: Mutations(sizeCell(n))})
		}
	}
	return append(sqlFixture(), MutateRows{CaseTable: true, Entries: entries})
}

// TotalsCases are the SQL cases for the totals view's query and the functions it uses.
func TotalsCases() []Case {
	const (
		rowA   = " FROM `{table}` WHERE _key = 't1#p1#n#rowA'"
		t1ToT4 = " FROM `{table}` WHERE _key >= 't1' AND _key < 't4'"
		sum    = "SUM(TO_INT64(size['bytes']))"
	)
	var sumCalls []Call
	for i := range sumGroups {
		sumCalls = append(sumCalls, Query(fmt.Sprintf("SELECT %s AS s FROM `{table}` WHERE STARTS_WITH(_key, 'sum#g%d#')", sum, i+1))...)
	}
	for _, sizes := range sumGroups {
		sumCalls = append(sumCalls, Query(fmt.Sprintf("SELECT SUM(x) AS s FROM `{table}`, UNNEST(%s) AS x WHERE _key = 't1#p1#n#rowA'", arrayLiteral(sizes)))...)
	}

	return []Case{
		{
			Name:  "SQL totals query",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query(totalsQuery+";"),
				Query(totalsQuery+"\nORDER BY tenantId, partitionId, rowType, labelId"),
			),
		},
		{
			Name:  "SQL JSON_QUERY_ARRAY",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query(`SELECT JSON_QUERY_ARRAY('["default","b2"]') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('["a\\"b"]') AS a`+rowA),
				Query("SELECT JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING)) AS a"+t1ToT4),
				Query(`SELECT JSON_QUERY_ARRAY('[1, 1.50, 1e2, true, null, {"b":1, "a":2}, [2, 3], "s"]') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('not json') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('{"a":1}') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('[]') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY(CAST(NULL AS STRING)) AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('[ "a" , "\\u0041", "é", "\\/" ]') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('"x"') AS a, JSON_QUERY_ARRAY('') AS b`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('[{"a":1,"a":2}]') AS a`+rowA),
				Query(`SELECT JSON_QUERY_ARRAY('[12345678901234567890, 0.1, -0, 1E400]') AS a`+rowA),
			),
		},
		{
			Name:  "SQL SPLIT, array subscripts, ARRAY_CONCAT, CASE and COALESCE",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query("SELECT SPLIT(_key, '#') AS a, SPLIT(b'a##b', b'#') AS b, SPLIT(b'', b'#') AS c"+rowA),
				Query("SELECT SPLIT(_key, b'') AS a"+rowA),
				Query("SELECT SPLIT(_key, '#')[OFFSET(1)] AS a"+rowA),
				Query("SELECT SPLIT(_key, '#')[-1] AS x"+rowA),
				Query("SELECT CAST(NULL AS ARRAY<BYTES>)[0] AS x"+rowA),
				Query("SELECT ARRAY_CONCAT(['$'], CAST(NULL AS ARRAY<STRING>)) AS a, ARRAY_CONCAT(['$'], []) AS b, ARRAY_CONCAT(['$'], COALESCE(CAST(NULL AS ARRAY<STRING>), [])) AS c"+rowA),
				Query("SELECT [] AS a"+rowA),
				Query("SELECT CASE WHEN '$' = 'x' THEN 1 ELSE 0 END AS a, CASE WHEN NULL THEN 1 END AS b"+rowA),
				Query("SELECT COALESCE(size['nope'], b'z') AS a, COALESCE(NULL, 'x') AS b"+rowA),
				Query("SELECT TO_INT64(b'1234567') AS n"+rowA),
			),
		},
		{
			Name:  "SQL UNNEST",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query("SELECT _key, x FROM `{table}`, UNNEST(JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING))) AS x WHERE _key >= 't1' AND _key < 't4'"),
				Query("SELECT x FROM `{table}`, UNNEST([3, 1, 2]) AS x WHERE _key = 't1#p1#n#rowA'"),
				Query("SELECT x IS NULL AS isnull, x FROM `{table}`, UNNEST(['a', NULL]) AS x WHERE _key = 't1#p1#n#rowA'"),
				Query(`SELECT x IS NULL AS isnull, x FROM `+"`{table}`"+`, UNNEST(JSON_QUERY_ARRAY('[null, "a"]')) AS x WHERE _key = 't1#p1#n#rowA'`),
				Query("SELECT _key, x FROM `{table}` CROSS JOIN UNNEST(['a']) AS x WHERE _key = 't1#p1#n#rowA'"),
			),
		},
		{
			Name:  "SQL GROUP BY and aggregates",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query("SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n"+t1ToT4+" GROUP BY 1 ORDER BY 1"),
				Query("SELECT SPLIT(_key, '#')[0] AS t, "+sum+" AS s"+t1ToT4+" GROUP BY t ORDER BY t"),
				Query("SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n"+t1ToT4+" GROUP BY t"),
				Query("SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n"+t1ToT4+" GROUP BY t HAVING COUNT(*) > 1 ORDER BY t"),
				Query("SELECT COUNT(*) AS n, "+sum+" AS s FROM `{table}` WHERE _key = 'nope'"),
				Query("SELECT _key, COUNT(*) AS n FROM `{table}` WHERE _key = 'nope' GROUP BY _key"),
				Query("SELECT JSON_QUERY_ARRAY(CAST(labels['labels'] AS STRING)) AS a, COUNT(*) AS n FROM `{table}` GROUP BY a"),
				Query("SELECT x, COUNT(*) AS n FROM `{table}`, UNNEST(['a', 'a', 'b']) AS x WHERE _key = 't1#p1#n#rowA' GROUP BY x ORDER BY x"),
				Query("SELECT k FROM (SELECT _key AS k FROM `{table}` WHERE _key >= 't1' AND _key < 't2') AS s ORDER BY k DESC"),
				Query("SELECT MAX(TO_INT64(size['bytes'])) AS mx, MAX(labels['labels']) AS ml"+t1ToT4),
			),
		},
		{
			Name:  "SQL SUM overflow on a running sum",
			Setup: sumFixture(),
			Calls: slices.Concat(
				Query("SELECT SPLIT(_key, '#')[1] AS g, "+sum+" AS s, COUNT(*) AS n FROM `{table}` WHERE STARTS_WITH(_key, 'sum#') GROUP BY g"),
				Query("SELECT SPLIT(_key, '#')[1] AS g, "+sum+" AS s FROM `{table}` WHERE STARTS_WITH(_key, 'sum#g2#') OR STARTS_WITH(_key, 'sum#g4#') GROUP BY g"),
				sumCalls,
			),
		},
	}
}

func arrayLiteral(ns []int64) string {
	s := "["
	for i, n := range ns {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprint(n)
	}
	return s + "]"
}
