// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

// sqlFixture is a server with a table t and a client connected to it. Table t has a plain family size and a Sum
// family total. Rows a, b and c hold size:bytes as 8 big-endian bytes, and row b also holds a total.
type sqlFixture struct {
	srv    *Server
	conn   *grpc.ClientConn
	client *bigtable.Client
	admin  *bigtable.AdminClient
}

func newSQLFixture(ctx context.Context, t *testing.T) *sqlFixture {
	t.Helper()
	srv, err := NewServer("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	admin, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	client, err := bigtable.NewClientWithConfig(ctx, "p", "i", bigtable.ClientConfig{MetricsProvider: bigtable.NoopMetricsProvider{}}, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	f := &sqlFixture{srv: srv, conn: conn, client: client, admin: admin}
	f.createTable(ctx, t, "t")
	return f
}

// createTable creates a table with the fixture's families and rows.
func (f *sqlFixture) createTable(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	sum := bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: bigtable.SumAggregator{}}
	if err := f.admin.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: name, ColumnFamilies: map[string]bigtable.Family{
		"size":  {GCPolicy: bigtable.NoGcPolicy()},
		"total": {GCPolicy: bigtable.NoGcPolicy(), ValueType: sum},
	}}); err != nil {
		t.Fatal(err)
	}
	tbl := f.client.Open(name)
	for key, size := range map[string]uint64{"a": 300, "b": 100, "c": 200} {
		m := bigtable.NewMutation()
		m.Set("size", "bytes", 0, binary.BigEndian.AppendUint64(nil, size))
		if key == "b" {
			m.AddIntToCell("total", "n", 0, 7)
		}
		if err := tbl.Apply(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}
}

// sqlResult is a query result as the Go client reads it.
type sqlResult struct {
	cols  []string
	types []bigtable.SQLType
	rows  [][]any
}

// query prepares and runs sql through the Go client.
func (f *sqlFixture) query(ctx context.Context, t *testing.T, sql string, types map[string]bigtable.SQLType, params map[string]any) sqlResult {
	t.Helper()
	ps, err := f.client.PrepareStatement(ctx, sql, types)
	if err != nil {
		t.Fatalf("PrepareStatement(%q): %v", sql, err)
	}
	return execute(ctx, t, ps, params)
}

// execute runs a prepared statement through the Go client. It takes the columns from the first row. It reads BYTES
// as []byte, INT64 and STRING as pointers, so a NULL reads as nil, and other types as the client's default.
func execute(ctx context.Context, t *testing.T, ps *bigtable.PreparedStatement, params map[string]any) sqlResult {
	t.Helper()
	bs, err := ps.Bind(params)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var res sqlResult
	if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool {
		if res.cols == nil {
			for _, c := range r.Metadata.Columns {
				res.cols = append(res.cols, c.Name)
				res.types = append(res.types, c.SQLType)
			}
		}
		var row []any
		for i, c := range r.Metadata.Columns {
			var err error
			switch c.SQLType.(type) {
			case bigtable.BytesSQLType:
				var v []byte
				err = r.GetByIndex(i, &v)
				row = append(row, v)
			case bigtable.Int64SQLType:
				var v *int64
				err = r.GetByIndex(i, &v)
				row = append(row, v)
			case bigtable.StringSQLType:
				var v *string
				err = r.GetByIndex(i, &v)
				row = append(row, v)
			default:
				var v any
				err = r.GetByIndex(i, &v)
				row = append(row, v)
			}
			if err != nil {
				t.Fatalf("GetByIndex(%d): %v", i, err)
			}
		}
		res.rows = append(res.rows, row)
		return true
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func i64(v int64) *int64 { return &v }

func sqlContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSQLSelectsRowWithTypedColumns(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key, TO_INT64(size['bytes']) AS bytes FROM t WHERE _key = 'b'", nil, nil)

	want := sqlResult{
		cols:  []string{"_key", "bytes"},
		types: []bigtable.SQLType{bigtable.BytesSQLType{}, bigtable.Int64SQLType{}},
		rows:  [][]any{{[]byte("b"), i64(100)}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(sqlResult{})); diff != "" {
		t.Errorf("result (-want +got):\n%s", diff)
	}
}

// keys returns the first column of each row.
func (r sqlResult) keys() []string {
	var out []string
	for _, row := range r.rows {
		out = append(out, string(row[0].([]byte)))
	}
	return out
}

func TestSQLOrderBySortsRows(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key, TO_INT64(size['bytes']) AS bytes FROM t ORDER BY bytes DESC", nil, nil)

	if diff := cmp.Diff([]string{"a", "c", "b"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLWhereMatchingNothingReturnsNoRows(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key FROM t WHERE _key = 'nope'", nil, nil)

	if len(got.rows) != 0 {
		t.Errorf("rows = %v, want none", got.rows)
	}
}

func TestSQLPrepareRejectsUnsupportedSQL(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	for _, tc := range []struct {
		sql, msg string
	}{
		// The analyzer's own error, as production words it.
		{"SELECT NOSUCHFN(_key) FROM t", "Function not found: NOSUCHFN [at 1:8]"},
		// A GoogleSQL function the emulator does not evaluate.
		{"SELECT LENGTH(_key) FROM t", "The emulator does not support LENGTH"},
		{"SELECT t._key FROM t, t AS u", "The emulator does not support JOIN"},
		{"SELECT _key FROM t LEFT JOIN UNNEST([1]) AS x", "The emulator does not support LEFT JOIN"},
		{"SELECT COUNT(_key) FROM t", "The emulator does not support COUNT"},
		{"SELECT _key FROM t LIMIT 1 OFFSET 1", "The emulator does not support OFFSET"},
		{"SELECT DISTINCT _key FROM t", "The emulator does not support SELECT DISTINCT"},
		{"SELECT _key FROM t WHERE _key IN (SELECT _key FROM t)", "The emulator does not support IN subqueries"},
		{"SELECT _key FROM t WHERE EXISTS (SELECT _key FROM t)", "The emulator does not support EXISTS subqueries"},
		// Production's message for a scalar subquery.
		{"SELECT (SELECT SUM(x) FROM UNNEST([1, 2]) AS x) AS s FROM t", "Subqueries are not supported"},
		{"SELECT STRUCT(1 AS a).a FROM t", "The emulator does not support STRUCT field access"},
		{`SELECT JSON_QUERY_ARRAY('{"a":["x"]}', '$.a') AS a FROM t`, "The emulator does not support JSON_QUERY_ARRAY with a JSONPath other than $"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			_, err := f.client.PrepareStatement(ctx, tc.sql, nil)
			wantStatus(t, err, codes.InvalidArgument, tc.msg)
		})
	}
}

func TestSQLPrepareRejectsUnsupportedParameterType(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	_, err := f.client.PrepareStatement(ctx, "SELECT _key FROM t WHERE @b", map[string]bigtable.SQLType{"b": bigtable.BoolSQLType{}})

	wantStatus(t, err, codes.InvalidArgument, "The emulator does not support query parameters of type bool_type")
}

func TestSQLReadsUnquotedDashedTableName(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.createTable(ctx, t, "items-prod")

	got := f.query(ctx, t, "SELECT _key FROM items-prod", nil, nil)

	if diff := cmp.Diff([]string{"a", "b", "c"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLInWithLimitReturnsFirstMatch(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key FROM t WHERE _key IN ('a', 'b') ORDER BY _key LIMIT 1", nil, nil)

	if diff := cmp.Diff([]string{"a"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLBoundParameterSelectsMatchingRow(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key FROM t WHERE _key = @k", map[string]bigtable.SQLType{"k": bigtable.BytesSQLType{}}, map[string]any{"k": []byte("c")})

	if diff := cmp.Diff([]string{"c"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLLikeBoundParameterPatternSelectsMatchingRow(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key FROM t WHERE CAST(_key AS STRING) LIKE @p", map[string]bigtable.SQLType{"p": bigtable.StringSQLType{}}, map[string]any{"p": "b%"})

	if diff := cmp.Diff([]string{"b"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLFiltersSelectMatchingRows(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	for _, tc := range []struct {
		name, where string
		want        []string
	}{
		{"BETWEEN includes both bounds", "_key BETWEEN 'a' AND 'b'", []string{"a", "b"}},
		{"BETWEEN with equal bounds matches the bound", "_key BETWEEN 'b' AND 'b'", []string{"b"}},
		{"BETWEEN with reversed bounds matches nothing", "_key BETWEEN 'c' AND 'a'", nil},
		{"BETWEEN with a NULL bound matches nothing", "_key BETWEEN NULL AND 'b'", nil},
		{"NOT BETWEEN with a NULL lower bound matches above the upper bound", "NOT (_key BETWEEN NULL AND 'b')", []string{"c"}},
		{"NOT BETWEEN with a NULL upper bound matches below the lower bound", "NOT (_key BETWEEN 'b' AND NULL)", []string{"a"}},
		// GoogleSQL defines BETWEEN as two comparisons under AND, so a false upper comparison decides a NULL lower bound.
		{"NOT BETWEEN with a NULL bound matches above the other bound", "TO_INT64(size['bytes']) NOT BETWEEN total['n'] AND 150", []string{"a", "c"}},
		{"LIKE underscore matches one byte", "_key LIKE '_'", []string{"a", "b", "c"}},
		{"LIKE percent matches a prefix", "CAST(_key AS STRING) LIKE 'b%'", []string{"b"}},
		{"STARTS_WITH matches a prefix", "STARTS_WITH(_key, 'c')", []string{"c"}},
		{"IS NULL matches a missing cell", "total['n'] IS NULL", []string{"a", "c"}},
		{"IS NOT NULL matches a present cell", "total['n'] IS NOT NULL", []string{"b"}},
		{"OR matches either side", "_key = 'a' OR total['n'] = 7", []string{"a", "b"}},
		{"AND matches both sides", "_key != 'a' AND TO_INT64(size['bytes']) >= 200", []string{"c"}},
		{"bytes literal matches a cell", "size['bytes'] = b'\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x64'", []string{"b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := "SELECT _key FROM t WHERE " + tc.where
			if diff := cmp.Diff(tc.want, f.query(ctx, t, sql, nil, nil).keys()); diff != "" {
				t.Errorf("%s: keys (-want +got):\n%s", sql, diff)
			}
		})
	}
}

func TestSQLSelectListTypesAliasesAndStar(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT CAST(_key AS STRING) AS k, total['n'] AS n, * FROM t WHERE _key = 'b'", nil, nil)

	b := "b"
	n := int64(7)
	want := sqlResult{
		cols: []string{"k", "n", "_key", "size", "total"},
		types: []bigtable.SQLType{
			bigtable.StringSQLType{}, bigtable.Int64SQLType{}, bigtable.BytesSQLType{},
			bigtable.MapSQLType{KeyType: bigtable.BytesSQLType{}, ValueType: bigtable.BytesSQLType{}},
			bigtable.MapSQLType{KeyType: bigtable.BytesSQLType{}, ValueType: bigtable.Int64SQLType{}},
		},
		// The client keys a BYTES map by the base64 of each key: Ynl0ZXM= is "bytes" and bg== is "n".
		rows: [][]any{{&b, &n, []byte("b"),
			map[string][]byte{"Ynl0ZXM=": binary.BigEndian.AppendUint64(nil, 100)},
			map[string]*int64{"bg==": &n},
		}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(sqlResult{})); diff != "" {
		t.Errorf("result (-want +got):\n%s", diff)
	}
}

// prepareRaw prepares sql through the raw PrepareQuery RPC and returns the prepared query.
func (f *sqlFixture) prepareRaw(ctx context.Context, t *testing.T, sql string) []byte {
	t.Helper()
	resp, err := btpb.NewBigtableClient(f.conn).PrepareQuery(ctx, &btpb.PrepareQueryRequest{InstanceName: "projects/p/instances/i", Query: sql})
	if err != nil {
		t.Fatalf("PrepareQuery(%q): %v", sql, err)
	}
	return resp.PreparedQuery
}

// executeRaw runs a prepared query through the raw ExecuteQuery RPC, and returns every response message and the
// stream's error.
func (f *sqlFixture) executeRaw(ctx context.Context, t *testing.T, prepared []byte) ([]*btpb.ExecuteQueryResponse, error) {
	t.Helper()
	stream, err := btpb.NewBigtableClient(f.conn).ExecuteQuery(ctx, &btpb.ExecuteQueryRequest{InstanceName: "projects/p/instances/i", PreparedQuery: prepared})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []*btpb.ExecuteQueryResponse
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return msgs, nil
		}
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, m)
	}
}

func TestSQLExecuteQuerySendsBatchThenToken(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	for _, tc := range []struct {
		name, sql string
		values    []*btpb.Value
	}{
		{"rows", "SELECT _key FROM t WHERE _key <= 'b'", []*btpb.Value{
			{Kind: &btpb.Value_BytesValue{BytesValue: []byte("a")}},
			{Kind: &btpb.Value_BytesValue{BytesValue: []byte("b")}},
		}},
		{"no rows", "SELECT _key FROM t WHERE _key = 'nope'", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := f.executeRaw(ctx, t, f.prepareRaw(ctx, t, tc.sql))
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			if len(msgs) != 2 {
				t.Fatalf("got %d messages, want 2: %v", len(msgs), msgs)
			}
			first, second := msgs[0].GetResults(), msgs[1].GetResults()
			batch := first.GetProtoRowsBatch()
			if batch == nil || !first.GetReset_() || first.ResumeToken != nil {
				t.Errorf("first message = %v, want a batch with reset and no token", first)
			}
			if tc.values == nil {
				if len(batch.GetBatchData()) != 0 || first.BatchChecksum != nil {
					t.Errorf("first message = %v, want an empty batch and no checksum", first)
				}
			} else {
				var rows btpb.ProtoRows
				if err := proto.Unmarshal(batch.GetBatchData(), &rows); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.values, rows.Values, protocmp.Transform()); diff != "" {
					t.Errorf("values (-want +got):\n%s", diff)
				}
				if want := crc32.Checksum(batch.GetBatchData(), crc32.MakeTable(crc32.Castagnoli)); first.GetBatchChecksum() != want || first.BatchChecksum == nil {
					t.Errorf("batch_checksum = %v, want %d", first.BatchChecksum, want)
				}
			}
			if second.GetPartialRows() != nil || second.GetReset_() || second.BatchChecksum != nil || len(second.GetResumeToken()) == 0 {
				t.Errorf("second message = %v, want only a resume token", second)
			}
		})
	}
}

func TestSQLExecuteQuerySplitsLargeResultIntoBatches(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	// Production's probe used 2000 rows of 4 KiB, about 8 MB.
	const nRows = 2000
	var keys []string
	var muts []*bigtable.Mutation
	for i := range nRows {
		keys = append(keys, fmt.Sprintf("big#%04d", i))
		m := bigtable.NewMutation()
		m.Set("size", "blob", 0, bytes.Repeat([]byte{'x'}, 4096))
		muts = append(muts, m)
	}
	// The server takes requests of up to 4 MiB, so write 500 rows at a time.
	for i := 0; i < nRows; i += 500 {
		if errs, err := f.client.Open("t").ApplyBulk(ctx, keys[i:i+500], muts[i:i+500]); err != nil || errs != nil {
			t.Fatalf("ApplyBulk: %v %v", err, errs)
		}
	}
	const sql = "SELECT _key, size FROM t WHERE STARTS_WITH(_key, 'big#')"

	msgs, err := f.executeRaw(ctx, t, f.prepareRaw(ctx, t, sql))

	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("got %d messages, want batches then a token", len(msgs))
	}
	const batchLimit = 2 << 20
	var gotKeys []string
	for i, m := range msgs[:len(msgs)-1] {
		r := m.GetResults()
		data := r.GetProtoRowsBatch().GetBatchData()
		if r.GetReset_() != (i == 0) || r.ResumeToken != nil || r.GetEstimatedBatchSize() != 0 {
			t.Errorf("message %d: reset = %v, resume_token = %q, estimated_batch_size = %d; want reset only on the first, no token, and size 0", i, r.GetReset_(), r.ResumeToken, r.GetEstimatedBatchSize())
		}
		if want := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)); r.BatchChecksum == nil || r.GetBatchChecksum() != want {
			t.Errorf("message %d: batch_checksum = %v, want %d", i, r.BatchChecksum, want)
		}
		var rows btpb.ProtoRows
		if err := proto.Unmarshal(data, &rows); err != nil {
			t.Fatal(err)
		}
		vals := rows.Values
		if len(vals) == 0 || len(vals)%2 != 0 {
			t.Fatalf("message %d holds %d values, want whole rows of 2", i, len(vals))
		}
		// A batch closes on the first row that brings it to 2 MiB or more.
		withoutLast, err := proto.Marshal(&btpb.ProtoRows{Values: vals[:len(vals)-2]})
		if err != nil {
			t.Fatal(err)
		}
		if len(withoutLast) >= batchLimit {
			t.Errorf("message %d: batch is %d bytes without its last row, want under %d", i, len(withoutLast), batchLimit)
		}
		if i < len(msgs)-2 && len(data) < batchLimit {
			t.Errorf("message %d: batch is %d bytes and not the last, want at least %d", i, len(data), batchLimit)
		}
		for j := 0; j < len(vals); j += 2 {
			gotKeys = append(gotKeys, string(vals[j].GetBytesValue()))
		}
	}
	if diff := cmp.Diff(keys, gotKeys); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
	if last := msgs[len(msgs)-1].GetResults(); last.GetPartialRows() != nil || last.GetReset_() || last.BatchChecksum != nil || len(last.GetResumeToken()) == 0 {
		t.Errorf("last message = %v, want only a resume token", last)
	}
	if got := f.query(ctx, t, sql, nil, nil); len(got.rows) != nRows {
		t.Errorf("Go client read %d rows, want %d", len(got.rows), nRows)
	}
}

// fakeClock is a clock that tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// useClock makes the server's SQL handlers read the time from a fake clock.
func (f *sqlFixture) useClock() *fakeClock {
	c := &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	f.srv.s.sql.mu.Lock()
	f.srv.s.sql.now = c.now
	f.srv.s.sql.mu.Unlock()
	return c
}

const expiredQuery = "The prepared query has expired. Please re-issue the ExecuteQuery with a valid prepared query."

// checkExpired checks that an ExecuteQuery failed as production fails an expired prepared query, before any
// message.
func checkExpired(t *testing.T, msgs []*btpb.ExecuteQueryResponse, err error, wantMsg string) {
	t.Helper()
	if len(msgs) != 0 {
		t.Errorf("got %d messages before the error, want none", len(msgs))
	}
	wantStatus(t, err, codes.FailedPrecondition, wantMsg)
	want := []any{&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{{Type: "PREPARED_QUERY_EXPIRED"}}}}
	if diff := cmp.Diff(want, status.Convert(err).Details(), protocmp.Transform()); diff != "" {
		t.Errorf("error details (-want +got):\n%s", diff)
	}
}

func TestSQLPreparedQueryExpiresAfter40Seconds(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	clock := f.useClock()
	prepared := f.prepareRaw(ctx, t, "SELECT _key FROM t")

	clock.advance(39 * time.Second)
	if _, err := f.executeRaw(ctx, t, prepared); err != nil {
		t.Fatalf("ExecuteQuery 39 s after prepare: %v", err)
	}
	clock.advance(2 * time.Second)
	msgs, err := f.executeRaw(ctx, t, prepared)
	checkExpired(t, msgs, err, expiredQuery)

	// A later PrepareQuery drops the expired query from the server, and the error stays the same.
	f.prepareRaw(ctx, t, "SELECT _key FROM t")
	msgs, err = f.executeRaw(ctx, t, prepared)
	checkExpired(t, msgs, err, expiredQuery)
}

func TestSQLExecuteQueryRejectsUnknownPreparedQuery(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	_, err := f.executeRaw(ctx, t, []byte("garbage"))

	wantStatus(t, err, codes.InvalidArgument, "Invalid prepared query. Please retry request with a valid prepared query")
}

func TestSQLPreparedQueryExpiresWhenItsFamilyIsDropped(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	readsTotal := f.prepareRaw(ctx, t, "SELECT * FROM t")
	readsNoFamily := f.prepareRaw(ctx, t, "SELECT _key FROM t WHERE _key = 'a'")

	if err := f.admin.DeleteColumnFamily(ctx, "t", "total"); err != nil {
		t.Fatal(err)
	}

	// Production doubles the message for a dropped family, and fails each later execute too.
	for range 2 {
		msgs, err := f.executeRaw(ctx, t, readsTotal)
		checkExpired(t, msgs, err, expiredQuery+" : "+expiredQuery)
	}
	if _, err := f.executeRaw(ctx, t, readsNoFamily); err != nil {
		t.Errorf("ExecuteQuery of a query that reads no family: %v", err)
	}
}

func TestSQLGoClientRepreparesAfterFamilyDrop(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	ps, err := f.client.PrepareStatement(ctx, "SELECT * FROM t WHERE _key = 'a'", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.admin.DeleteColumnFamily(ctx, "t", "total"); err != nil {
		t.Fatal(err)
	}
	bs, err := ps.Bind(nil)
	if err != nil {
		t.Fatal(err)
	}

	var cols []string
	err = bs.Execute(ctx, func(r bigtable.ResultRow) bool {
		for _, c := range r.Metadata.Columns {
			cols = append(cols, c.Name)
		}
		return true
	})

	if err != nil {
		t.Fatalf("Execute after the drop: %v", err)
	}
	if diff := cmp.Diff([]string{"_key", "size"}, cols); diff != "" {
		t.Errorf("columns (-want +got):\n%s", diff)
	}
}

func TestSQLMixedCaseParameterSelectsMatchingRow(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key FROM t WHERE _key = @Key", map[string]bigtable.SQLType{"Key": bigtable.BytesSQLType{}}, map[string]any{"Key": []byte("c")})

	if diff := cmp.Diff([]string{"c"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLPrepareIgnoresTableWhoseNameDiffersOnlyInCase(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.createTable(ctx, t, "T")

	got := f.query(ctx, t, "SELECT _key FROM t", nil, nil)

	if diff := cmp.Diff([]string{"a", "b", "c"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLPrepareAcceptsFamiliesThatDifferOnlyInCase(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	if err := f.admin.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "u", ColumnFamilies: map[string]bigtable.Family{
		"size": {GCPolicy: bigtable.NoGcPolicy()},
		"Size": {GCPolicy: bigtable.NoGcPolicy()},
	}}); err != nil {
		t.Fatal(err)
	}

	for _, sql := range []string{"SELECT _key FROM t", "SELECT _key FROM u"} {
		if _, err := f.client.PrepareStatement(ctx, sql, nil); err != nil {
			t.Errorf("PrepareStatement(%q): %v", sql, err)
		}
	}
}

func TestSQLLimitInSubqueryKeepsOuterOrderBy(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	got := f.query(ctx, t, "SELECT _key FROM (SELECT _key FROM t ORDER BY _key LIMIT 2) ORDER BY _key DESC", nil, nil)

	if diff := cmp.Diff([]string{"b", "a"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

// addKeys writes a size cell to each row key in table t.
func (f *sqlFixture) addKeys(ctx context.Context, t *testing.T, keys ...string) {
	t.Helper()
	tbl := f.client.Open("t")
	for _, key := range keys {
		m := bigtable.NewMutation()
		m.Set("size", "bytes", 0, binary.BigEndian.AppendUint64(nil, 1))
		if err := tbl.Apply(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLLikeOnKeyMatchesBytes(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.addKeys(ctx, t, "k#%", `k#\`, "k#_x", "k#a", "k#ax", "k#é", "k#€", "k#\xff")

	for _, tc := range []struct {
		name, pattern string
		want          []string
	}{
		{"escaped percent matches only a percent", `r'k#\%'`, []string{"k#%"}},
		{"escaped underscore matches only an underscore", `r'k#\_x'`, []string{"k#_x"}},
		{"escaped ordinary byte matches that byte", `r'k#\a'`, []string{"k#a"}},
		{"underscore matches one byte", `'k#_'`, []string{"k#%", `k#\`, "k#a", "k#\xff"}},
		{"two underscores match a two-byte character", `'k#__'`, []string{"k#_x", "k#ax", "k#é"}},
		{"three underscores match a three-byte character", `'k#___'`, []string{"k#€"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := "SELECT _key FROM t WHERE _key LIKE " + tc.pattern
			if diff := cmp.Diff(tc.want, f.query(ctx, t, sql, nil, nil).keys()); diff != "" {
				t.Errorf("%s: keys (-want +got):\n%s", sql, diff)
			}
		})
	}
}

func TestSQLLikeUnderscoreMatchesOneCharacterOfString(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)
	f.addKeys(ctx, t, "k#a", "k#ax", "k#é", "k#€")

	got := f.query(ctx, t, "SELECT _key FROM t WHERE CAST(_key AS STRING) LIKE 'k#_'", nil, nil)

	if diff := cmp.Diff([]string{"k#a", "k#é", "k#€"}, got.keys()); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestSQLLikePatternEndingWithBackslashFailsAtExecute(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	for _, tc := range []struct {
		name, sql string
		types     map[string]bigtable.SQLType
		params    map[string]any
	}{
		{"literal", `SELECT _key FROM t WHERE _key LIKE 'a\\'`, nil, nil},
		{"BYTES parameter", "SELECT _key FROM t WHERE _key LIKE @p", map[string]bigtable.SQLType{"p": bigtable.BytesSQLType{}}, map[string]any{"p": []byte(`a\`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := f.client.PrepareStatement(ctx, tc.sql, tc.types)
			if err != nil {
				t.Fatalf("PrepareStatement(%q): %v", tc.sql, err)
			}
			bs, err := ps.Bind(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			err = bs.Execute(ctx, func(bigtable.ResultRow) bool { return true })
			wantStatus(t, err, codes.OutOfRange, "LIKE pattern ends with a backslash")
		})
	}
}

func TestSQLPrepareRejectsStringPatternForKeyLike(t *testing.T) {
	ctx := sqlContext(t)
	f := newSQLFixture(ctx, t)

	_, err := f.client.PrepareStatement(ctx, "SELECT _key FROM t WHERE _key LIKE @p", map[string]bigtable.SQLType{"p": bigtable.StringSQLType{}})

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v, want %v", got, codes.InvalidArgument)
	}
	// The lines after the first list the signatures the analyzer tried.
	first, _, _ := strings.Cut(status.Convert(err).Message(), "\n")
	if want := "No matching signature for operator LIKE for argument types: BYTES, STRING"; first != want {
		t.Errorf("first line = %q, want %q", first, want)
	}
}

func TestSQLPreparedQueryReadsFamilyReaddedBeforeExecute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family bigtable.Family
		write  func(*bigtable.Mutation)
		want   []byte
	}{
		{"plain", bigtable.Family{GCPolicy: bigtable.NoGcPolicy()}, func(m *bigtable.Mutation) { m.Set("size", "bytes", 0, []byte("after")) }, []byte("after")},
		// The query keeps its prepared type, so it reads the aggregate as its stored bytes.
		{"int64 Sum", bigtable.Family{GCPolicy: bigtable.NoGcPolicy(), ValueType: bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: bigtable.SumAggregator{}}},
			func(m *bigtable.Mutation) { m.AddIntToCell("size", "bytes", 0, 5) }, binary.BigEndian.AppendUint64(nil, 5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := sqlContext(t)
			f := newSQLFixture(ctx, t)
			const sql = "SELECT _key, size FROM t WHERE _key = 'b'"
			ps, err := f.client.PrepareStatement(ctx, sql, nil)
			if err != nil {
				t.Fatal(err)
			}
			// The client re-prepares an expired query, so only a raw execute shows that the query did not expire.
			token := f.prepareRaw(ctx, t, sql)
			if err := f.admin.DeleteColumnFamily(ctx, "t", "size"); err != nil {
				t.Fatal(err)
			}
			if err := f.admin.CreateColumnFamilyWithConfig(ctx, "t", "size", tc.family); err != nil {
				t.Fatal(err)
			}
			m := bigtable.NewMutation()
			tc.write(m)
			if err := f.client.Open("t").Apply(ctx, "b", m); err != nil {
				t.Fatal(err)
			}

			if _, err := f.executeRaw(ctx, t, token); err != nil {
				t.Fatalf("ExecuteQuery of the raw prepared query: %v", err)
			}
			got := execute(ctx, t, ps, nil)

			want := sqlResult{
				cols:  []string{"_key", "size"},
				types: []bigtable.SQLType{bigtable.BytesSQLType{}, bigtable.MapSQLType{KeyType: bigtable.BytesSQLType{}, ValueType: bigtable.BytesSQLType{}}},
				// Ynl0ZXM= is the base64 of "bytes".
				rows: [][]any{{[]byte("b"), map[string][]byte{"Ynl0ZXM=": tc.want}}},
			}
			if diff := cmp.Diff(want, got, cmp.AllowUnexported(sqlResult{})); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
		})
	}
}
