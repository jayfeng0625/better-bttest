// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"slices"
	"time"
)

// A view case waits on production's create, which takes 1 to 2 minutes, before its calls.
const viewDeadline = 5 * time.Minute

// expiredViewQuery lists the rows whose marker is unset, keyed by the source key under an alias.
const expiredViewQuery = "SELECT _key AS rowKey FROM `{table}` WHERE mark['marker'] IS NULL ORDER BY rowKey"

// viewFixture writes the SQL fixture, then creates a view of query over it. Production refreshes a view 3 to 6 s
// after a source write, so a case writes every source row before the create and reads a view that holds them.
func viewFixture(query string) []Call {
	return append(sqlFixture(), CreateView{Query: query})
}

// ViewCases are the cases that create a materialized view on the case's table. TestParity runs them only with
// -views. A case that reads a view ends with the admin calls that need a live view, so that the run creates as few
// views as it can.
func ViewCases() []Case {
	const (
		usage    = "SELECT tenantId, SUM(tenantPartitionType_itemCount) AS itemCount, SUM(tenantPartitionType_bytes) AS bytes FROM `{view}` WHERE labelId = '$'"
		bySizeID = "SELECT TO_INT64(size['bytes']) AS sz, _key AS rk FROM `{table}`"
		grouped  = "SELECT SPLIT(_key, '#')[0] AS t, COUNT(*) AS n FROM `{table}` GROUP BY t"
	)
	rejects := func(name string, queries ...string) Case {
		c := Case{Name: name, Setup: sqlFixture()}
		for _, q := range queries {
			c.Calls = append(c.Calls, CreateView{Query: q})
		}
		return c
	}

	cases := []Case{
		{
			Name:  "view of the totals query, then DeleteTable under it",
			Setup: viewFixture(totalsQuery),
			Calls: slices.Concat(
				[]Call{GetView{}, ReadView{}},
				Query("SELECT * FROM `{view}`"),
				Query("SELECT _key FROM `{view}`"),
				Query(usage+" GROUP BY tenantId"),
				Query(usage+" GROUP BY tenantId ORDER BY tenantId"),
				Query(usage+" AND tenantId = @t GROUP BY tenantId", BytesParam("t", []byte("t1"))),
				Query("SELECT tenantId, labelId, rowType FROM `{view}` WHERE tenantId = b't1' ORDER BY labelId DESC"),
				Query("SELECT tenantId FROM `{view}` WHERE tenantId = 't2'"),
				Query("SELECT tenantId FROM `{view}` WHERE tenantId = @t", StringParam("t", "t2")),
				Query("SELECT * FROM `{view}` LIMIT 2"),
				[]Call{DeleteTable{}, GetView{}, DeleteView{}, DeleteTable{}},
			),
		},
		{
			Name:  "view of the rows with no marker, then its deletion protection",
			Setup: viewFixture(expiredViewQuery),
			Calls: slices.Concat(
				[]Call{ReadView{}},
				Query("SELECT * FROM `{view}` WHERE rowKey >= b't1' AND rowKey < b't4'"),
				Query("SELECT _key FROM `{view}`"),
				Query("SELECT rowKey FROM `{view}` WHERE rowKey >= b't1' AND rowKey < b't4' ORDER BY rowKey DESC"),
				Query("SELECT COUNT(*) AS n FROM `{view}` WHERE rowKey >= b't1' AND rowKey < b't4'"),
				Query("SELECT * FROM `{view}`(with_history => TRUE)"),
				Query("SELECT _timestamp FROM `{view}`"),
				[]Call{SetViewDeletionProtection{On: true}, GetView{}, DeleteView{}, SetViewDeletionProtection{On: false}, DeleteView{}, GetView{}},
			),
		},
		{
			Name:  "view ordered by a size, then the key",
			Setup: viewFixture(bySizeID + " ORDER BY sz, rk"),
			Calls: slices.Concat([]Call{ReadView{}}, Query("SELECT * FROM `{view}`")),
		},
		{
			Name:  "view keyed by the source key alone",
			Setup: viewFixture("SELECT _key, TO_INT64(size['bytes']) AS sz FROM `{table}` ORDER BY _key"),
			Calls: slices.Concat([]Call{ReadView{}}, Query("SELECT * FROM `{view}`"), Query("SELECT sz FROM `{view}`")),
		},
		rejects("view create rejects nested or mixed GROUP BY and ORDER BY",
			"SELECT t, n FROM ("+grouped+") ORDER BY n",
			"SELECT n, COUNT(*) AS c FROM ("+grouped+") GROUP BY n",
			"SELECT t, COUNT(*) AS n FROM (SELECT SPLIT(_key, '#')[0] AS t FROM `{table}` ORDER BY t) GROUP BY t",
			grouped+" ORDER BY t",
		),
		rejects("view create rejects a key that is not selected or not the source key",
			bySizeID+" ORDER BY sz",
			"SELECT TO_INT64(size['bytes']) AS sz FROM `{table}` ORDER BY sz, _key",
			"SELECT SPLIT(_key, '#')[0] AS t, _key AS rk FROM `{table}` ORDER BY t, SPLIT(_key, '#')[1]",
			"SELECT COUNT(*) AS n FROM `{table}` GROUP BY SPLIT(_key, '#')[0]",
			"SELECT _key, TO_INT64(size['bytes']) AS sz FROM `{table}` GROUP BY _key, sz",
			"SELECT SPLIT(_key, '#')[0] AS t, MAX(_key) AS _key, COUNT(*) AS n FROM `{table}` GROUP BY t",
		),
		rejects("view create rejects volatile and unstable functions",
			"SELECT SPLIT(_key, '#')[0] AS t, CURRENT_TIMESTAMP() AS ts, COUNT(*) AS n FROM `{table}` GROUP BY t",
			"SELECT SPLIT(_key, '#')[0] AS t, RAND() AS r, COUNT(*) AS n FROM `{table}` GROUP BY t",
			"SELECT SPLIT(_key, '#')[0] AS t, GENERATE_UUID() AS u, COUNT(*) AS n FROM `{table}` GROUP BY t",
			"SELECT SPLIT(_key, '#')[0] AS t, CURRENT_DATE() AS d, COUNT(*) AS n FROM `{table}` GROUP BY t",
			"SELECT SPLIT(_key, '#')[0] AS t, ARRAY_AGG(_key) AS ks, COUNT(*) AS n FROM `{table}` GROUP BY t",
			"SELECT SPLIT(_key, '#')[0] AS t, STRING_AGG(_key) AS ks, COUNT(*) AS n FROM `{table}` GROUP BY t",
		),
		{
			Name:  "view admin calls on a view that does not exist",
			Calls: []Call{GetView{}, SetViewDeletionProtection{On: true}, SetViewDeletionProtection{On: false}, DeleteView{}},
		},
	}
	for i := range cases {
		cases[i].Deadline = viewDeadline
	}
	return cases
}
