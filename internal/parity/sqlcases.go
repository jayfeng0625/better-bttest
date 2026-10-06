// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// The SQL fixture's families, on the case's table.
const (
	Size   Family = "size"
	Labels Family = "labels"
	Mark   Family = "mark"
)

// A case that drops a family, or re-creates a table it deleted, needs longer than caseDeadline. Production's drop takes
// 30 to 32 s, and a table created under a deleted table's name was still being created 30 s later.
const adminDeadline = 90 * time.Second

// The marker times, in milliseconds: one year after the fixture was first written on production, and one hour before.
const (
	markAhead  = 1822572691361
	markBehind = 1791033091361
)

// The SQL cases name the case's table in backquotes. The run id starts with the Unix time in hex, so with a digit
// until 2055, and GoogleSQL does not parse an unquoted dashed name with a part that starts with a digit.
func keysWhere(cond string, params ...Param) []Call {
	return Query("SELECT _key FROM `{table}` WHERE "+cond, params...)
}

func sqlFamilies() map[string]*adminpb.ColumnFamily {
	return map[string]*adminpb.ColumnFamily{string(Size): {}, string(Labels): {}, string(Mark): {}}
}

func sizeCell(n int64) *btpb.Mutation    { return SetCell(Size, BE(n), Col("bytes")) }
func labelsCell(s string) *btpb.Mutation { return SetCell(Labels, []byte(s), Col("labels")) }
func markCell(ms int64) *btpb.Mutation   { return SetCell(Mark, BE(ms), Col("marker")) }
func mutateRowA(m *btpb.Mutation) MutateRow {
	return MutateRow{CaseTable: true, Row: "t1#p1#n#rowA", Mutations: Mutations(m)}
}

// sqlFixture creates the case's table with the families size, labels and mark, each with GC rule never (fixture.go
// says why), and writes the six rows every SQL case queries.
func sqlFixture() []Call {
	return []Call{
		CreateTable{Families: sqlFamilies()},
		MutateRows{CaseTable: true, Entries: []Entry{
			{Row: "t1#p1#n#rowA", Mutations: Mutations(sizeCell(100), labelsCell(`["default","b2"]`), markCell(markAhead))},
			{Row: "t1#p1#n#rowB", Mutations: Mutations(sizeCell(50), labelsCell(`["default"]`))},
			{Row: "t1#p1#b#rowC", Mutations: Mutations(sizeCell(70), markCell(markAhead))},
			{Row: "t2#p1#n#rowD", Mutations: Mutations(sizeCell(10), labelsCell(`[]`))},
			{Row: "t3#p1#m#rowE", Mutations: Mutations(labelsCell(`["a\"b"]`), markCell(markAhead))},
			{Row: "t3#p1#n#rowF", Mutations: Mutations(sizeCell(5), labelsCell(`["default"]`), markCell(markBehind))},
		}},
	}
}

// The SQL fixture, and the rows like# followed by each of the suffixes that LIKE and BETWEEN tell apart.
func likeFixture() []Call {
	var entries []Entry
	for _, s := range []string{"", "a", "b", "ax", "_x", "é", "€", `\`, "%", "\xff"} {
		entries = append(entries, Entry{Row: Row("like#" + s), Mutations: Mutations(sizeCell(1))})
	}
	return append(sqlFixture(), MutateRows{CaseTable: true, Entries: entries})
}

// SQLCases are the cases that query a case's table with PrepareQuery and ExecuteQuery.
func SQLCases() []Case {
	const (
		rowA     = "_key = 't1#p1#n#rowA'"
		t1ToT4   = "_key >= 't1' AND _key < 't4'"
		starRowA = "SELECT * FROM `{table}` WHERE " + rowA
		keyRowA  = "SELECT _key FROM `{table}` WHERE " + rowA
		markRowA = "SELECT _key, mark FROM `{table}` WHERE " + rowA
		sizeRowA = "SELECT _key, size FROM `{table}` WHERE " + rowA
		likeRows = "STARTS_WITH(_key, 'like#') AND "
	)
	dropMark := ModifyColumnFamilies{Mods: []*adminpb.ModifyColumnFamiliesRequest_Modification{DropFamily(string(Mark))}}

	// A family dropped and re-added as cf before the query that reads it first runs, then a fresh prepare.
	readded := func(name string, cf *adminpb.ColumnFamily, write *btpb.Mutation) Case {
		return Case{
			Name:     "SQL query prepared before its family is dropped and re-added as " + name,
			Setup:    sqlFixture(),
			Deadline: adminDeadline,
			Calls: slices.Concat([]Call{
				PrepareQuery{SQL: markRowA},
				dropMark,
				ModifyColumnFamilies{Mods: []*adminpb.ModifyColumnFamiliesRequest_Modification{CreateFamily(string(Mark), cf)}},
				mutateRowA(write),
				ExecuteQuery{},
			}, Query(markRowA)),
		}
	}
	// The table deleted after prepare and re-created with size as cf, then a fresh prepare.
	recreated := func(name string, cf *adminpb.ColumnFamily, write *btpb.Mutation) Case {
		return Case{
			Name:     "SQL query prepared before its table is deleted and re-created with the family as " + name,
			Setup:    sqlFixture(),
			Deadline: adminDeadline,
			Calls: slices.Concat([]Call{
				PrepareQuery{SQL: sizeRowA},
				DeleteTable{},
				CreateTable{Families: map[string]*adminpb.ColumnFamily{string(Size): cf}},
				mutateRowA(write),
				ExecuteQuery{},
			}, Query(sizeRowA)),
		}
	}

	big := make([]Entry, 1250)
	label := labelsCell(strings.Repeat("x", 4096))
	for i := range big {
		big[i] = Entry{Row: Row(fmt.Sprintf("big#%04d", i)), Mutations: Mutations(label)}
	}

	return []Case{
		{
			Name:  "SQL key filters, order and limit",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				keysWhere(t1ToT4),
				keysWhere(t1ToT4+" ORDER BY _key"),
				keysWhere(t1ToT4+" ORDER BY _key DESC"),
				keysWhere("STARTS_WITH(_key, 't3')"),
				keysWhere(t1ToT4+" AND mark['marker'] IS NULL"),
				keysWhere("(_key >= 't1' AND _key < 't2') OR (_key >= 't3' AND mark['marker'] IS NOT NULL AND _key < 't4')"),
				keysWhere(t1ToT4+" AND TO_INT64(size['bytes']) > 60"),
				keysWhere(`labels['labels'] = '["default"]'`),
				keysWhere(t1ToT4+" LIMIT 2"),
				keysWhere(t1ToT4+" LIMIT 0"),
				keysWhere(t1ToT4+" LIMIT @n", Int64Param("n", 3)),
				keysWhere("_key = @k", BytesParam("k", []byte("t1#p1#n#rowA"))),
				keysWhere("_key = 'nope'"),
			),
		},
		{
			Name:  "SQL select list, star, maps and casts",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query(starRowA),
				Query("SELECT _key, labels, size FROM `{table}` WHERE "+t1ToT4),
				Query("SELECT _key, size['bytes'] AS raw, TO_INT64(size['bytes']) AS bytes, CAST(labels['labels'] AS STRING) AS labels, mark['marker'] AS marker FROM `{table}` WHERE "+t1ToT4),
				Query("SELECT _key, TO_INT64(size['bytes']) AS bytes FROM `{table}` WHERE "+rowA),
				Query("SELECT _key, TO_INT64(size['bytes']) AS bytes, FROM `{table}` WHERE "+rowA),
				Query("SELECT _key, 1+1, SPLIT(_key, '#')[0] FROM `{table}` WHERE "+rowA),
				Query("SELECT _key, TO_INT64(size['bytes']) AS n FROM `{table}` WHERE _key = 't3#p1#m#rowE'"),
				Query("SELECT CAST(_key AS STRING) AS k, CAST(CAST(_key AS STRING) AS BYTES) AS b FROM `{table}` WHERE "+rowA),
				Query("SELECT _key, size['nope'] AS v FROM `{table}` WHERE "+rowA),
				Query("SELECT _key, labels IS NULL AS n, ARRAY_LENGTH(MAP_KEYS(labels)) AS k FROM `{table}` WHERE _key = 't1#p1#b#rowC'"),
				Query("SELECT * FROM `{table}` LIMIT 1"),
				Query("SELECT COUNT(*) AS n FROM `{table}` WHERE "+t1ToT4),
				Query("SELECT COUNT(*) AS n FROM `{table}`"),
			),
		},
		{
			Name:  "SQL errors at prepare",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				keysWhere("_key = @k", StringParam("k", "t1#p1#n#rowA")),
				Query("SELECT _key FROM nosuchtable"),
				Query("SELECT _key FROM no-such-table"),
				Query("SELEC _key FROM `{table}`"),
				Query("SELECT NOSUCHFN(_key) FROM `{table}`"),
				Query("SELECT nosuch['x'] FROM `{table}`"),
				keysWhere("_key = @k"),
				Query("SELECT _key FROM nosuch-table"),
				Query("SELECT _key FROM `nosuch`"),
				Query("SELECT _key FROM BETTER-BTTEST-PARITY WHERE "+rowA),
				Query("SELECT _key FROM {table} WHERE "+rowA),
				Query(keyRowA),
			),
		},
		{
			Name:  "SQL errors at execute",
			Setup: sqlFixture(),
			Calls: slices.Concat(
				Query("SELECT _key, TO_INT64(labels['labels']) AS n FROM `{table}` WHERE _key = 't1#p1#n#rowB'"),
				Query("SELECT SPLIT(_key, '#')[9] AS x FROM `{table}` WHERE "+rowA),
			),
		},
		{
			Name:  "SQL LIKE on the row key",
			Setup: likeFixture(),
			Calls: slices.Concat(
				keysWhere(`_key LIKE 'like#_'`),
				keysWhere(`_key LIKE b'like#_'`),
				keysWhere(`_key LIKE 'like#__'`),
				keysWhere(`_key LIKE 'like#___'`),
				keysWhere(`_key LIKE 'like#%'`),
				keysWhere(`_key LIKE '%like#_x'`),
				keysWhere(`_key LIKE 'like#\\_x'`),
				keysWhere(`_key LIKE r'like#\_x'`),
				keysWhere(`_key LIKE 'like#\\'`),
				keysWhere(`_key LIKE b'like#\\'`),
				keysWhere(`_key LIKE r'like#\'`),
				keysWhere(`_key LIKE 'like#\\\\'`),
				keysWhere(`_key LIKE 'like#\\%'`),
				keysWhere(`_key LIKE 'like#\\a'`),
				keysWhere(`_key LIKE 'LIKE#a'`),
				keysWhere(`_key LIKE @p`, BytesParam("p", []byte(`like#_`))),
				keysWhere(`_key LIKE @p`, BytesParam("p", []byte(`like#\`))),
				keysWhere(`_key LIKE @p`, StringParam("p", `like#_`)),
				keysWhere(`_key LIKE @p`, NullParam("p", BytesType)),
				keysWhere(likeRows+`CAST(_key AS STRING) LIKE 'like#_'`),
			),
		},
		{
			Name:  "SQL BETWEEN on the row key",
			Setup: likeFixture(),
			Calls: slices.Concat(
				keysWhere(`_key BETWEEN b'like#a' AND b'like#b'`),
				keysWhere(`_key BETWEEN 'like#a' AND 'like#b'`),
				keysWhere(`_key BETWEEN 'like#a' AND 'like#a'`),
				keysWhere(`_key BETWEEN 'like#b' AND 'like#a'`),
				keysWhere(`_key BETWEEN @lo AND @hi`, BytesParam("lo", []byte("like#a")), BytesParam("hi", []byte("like#b"))),
				keysWhere(`_key BETWEEN @lo AND @hi`, NullParam("lo", BytesType), BytesParam("hi", []byte("like#b"))),
				keysWhere(`_key BETWEEN @lo AND 'like#b'`, NullParam("lo", BytesType)),
				keysWhere(`_key BETWEEN 'like#a' AND @hi`, NullParam("hi", BytesType)),
				keysWhere(likeRows+`NOT (_key BETWEEN @lo AND 'like#b')`, NullParam("lo", BytesType)),
				keysWhere(likeRows+`NOT (_key BETWEEN 'like#a' AND @hi)`, NullParam("hi", BytesType)),
				keysWhere(likeRows+`_key NOT BETWEEN 'like#a' AND 'like#b'`),
				keysWhere(`_key BETWEEN NULL AND 'like#b'`),
				[]Call{
					PrepareQuery{SQL: "SELECT _key FROM `{table}` WHERE _key BETWEEN @lo AND 'like#b'", Params: map[string]*btpb.Type{"lo": BytesType}},
					ExecuteQuery{Params: map[string]*btpb.Value{}},
				},
			),
		},
		{
			Name:  "SQL result over several batches",
			Setup: []Call{CreateTable{Families: sqlFamilies()}, MutateRows{CaseTable: true, Entries: big}},
			Calls: Query("SELECT _key, labels FROM `{table}`"),
		},
		{
			Name:  "SQL query prepared before a family is added",
			Setup: sqlFixture(),
			Calls: slices.Concat([]Call{PrepareQuery{SQL: starRowA}, ModifyColumnFamilies{}, ExecuteQuery{}}, Query(starRowA)),
		},
		{
			Name:     "SQL query that reads a family dropped after prepare",
			Setup:    sqlFixture(),
			Deadline: adminDeadline,
			Calls:    []Call{PrepareQuery{SQL: starRowA}, dropMark, ExecuteQuery{}, ExecuteQuery{}},
		},
		{
			Name:     "SQL query that does not read a family dropped after prepare",
			Setup:    sqlFixture(),
			Deadline: adminDeadline,
			Calls:    []Call{PrepareQuery{SQL: keyRowA}, dropMark, ExecuteQuery{}},
		},
		readded("plain", &adminpb.ColumnFamily{}, SetCell(Mark, []byte("after"), Col("marker"))),
		readded("Sum", families()[string(Sum)], AddToCell(Mark, Int(5), Col("marker"))),
		{
			Name:  "SQL query prepared before its table is deleted",
			Setup: sqlFixture(),
			Calls: []Call{PrepareQuery{SQL: sizeRowA}, DeleteTable{}, ExecuteQuery{}},
		},
		recreated("plain", &adminpb.ColumnFamily{}, SetCell(Size, BE(7), Col("bytes"))),
		recreated("Sum", families()[string(Sum)], AddToCell(Size, Int(7), Col("bytes"))),
	}
}
