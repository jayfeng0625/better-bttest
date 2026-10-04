// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"slices"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/btree"
	"github.com/jayfeng0625/better-bttest/bttest/internal/sqlengine"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// preparedValidFor is how far ahead PrepareQuery sets valid_until, as production does.
	preparedValidFor = 10 * time.Second
	// preparedExpiresAfter is how long after PrepareQuery production stops executing a prepared query.
	preparedExpiresAfter = 40 * time.Second
	// expiredMessage is production's message for an expired prepared query.
	expiredMessage = "The prepared query has expired. Please re-issue the ExecuteQuery with a valid prepared query."
)

// resumeToken ends every ExecuteQuery result. ExecuteQuery sends no token before the end of a result, so no client
// resumes from it.
var resumeToken = []byte("better-bttest-resume-token")

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// preparedTokenPrefix starts every prepared_query the server returns.
const preparedTokenPrefix = "bbtq1:"

// sqlQueries holds the server's prepared queries, keyed by their prepared_query bytes. PrepareQuery drops the
// expired ones.
type sqlQueries struct {
	mu      sync.Mutex
	now     func() time.Time // nil means time.Now
	queries map[string]*preparedQuery
}

type preparedQuery struct {
	query      *sqlengine.Query
	instance   string
	paramTypes map[string]*btpb.Type
	params     map[string]sqlengine.Type
	prepared   time.Time
	// familyDropped is set on the first execute after a family the query reads is dropped.
	familyDropped bool
}

// newPreparedToken returns a new prepared_query: the prefix, the prepare time in big-endian Unix nanoseconds, and
// random text. The time lets ExecuteQuery tell an expired query from an unknown one after PrepareQuery drops it.
func newPreparedToken(prepared time.Time) []byte {
	token := binary.BigEndian.AppendUint64([]byte(preparedTokenPrefix), uint64(prepared.UnixNano()))
	return append(token, rand.Text()...)
}

func preparedAt(token []byte) (time.Time, bool) {
	rest, ok := bytes.CutPrefix(token, []byte(preparedTokenPrefix))
	if !ok || len(rest) < 8 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(rest))), true
}

func expired(prepared, now time.Time) bool {
	return !now.Before(prepared.Add(preparedExpiresAfter))
}

func (sq *sqlQueries) clock() time.Time {
	if sq.now != nil {
		return sq.now()
	}
	return time.Now()
}

func (s *server) PrepareQuery(ctx context.Context, req *btpb.PrepareQueryRequest) (*btpb.PrepareQueryResponse, error) {
	params := map[string]sqlengine.Type{}
	for name, t := range req.ParamTypes {
		pt, err := sqlengine.TypeFromProto(t)
		if err != nil {
			return nil, err
		}
		params[name] = pt
	}
	q, err := sqlengine.Prepare(req.Query, s.sqlTables(req.InstanceName), params)
	if err != nil {
		return nil, err
	}
	s.sql.mu.Lock()
	now := s.sql.clock()
	token := newPreparedToken(now)
	if s.sql.queries == nil {
		s.sql.queries = map[string]*preparedQuery{}
	}
	for k, pq := range s.sql.queries {
		if expired(pq.prepared, now) {
			delete(s.sql.queries, k)
		}
	}
	s.sql.queries[string(token)] = &preparedQuery{query: q, instance: req.InstanceName, paramTypes: req.ParamTypes, params: params, prepared: now}
	s.sql.mu.Unlock()

	cols := make([]*btpb.ColumnMetadata, len(q.Columns))
	for i, c := range q.Columns {
		cols[i] = &btpb.ColumnMetadata{Name: c.Name, Type: c.Type.Proto()}
	}
	return &btpb.PrepareQueryResponse{
		Metadata:      &btpb.ResultSetMetadata{Schema: &btpb.ResultSetMetadata_ProtoSchema{ProtoSchema: &btpb.ProtoSchema{Columns: cols}}},
		PreparedQuery: token,
		ValidUntil:    timestamppb.New(now.Add(preparedValidFor)),
	}, nil
}

// sqlTables lists the instance's tables as SQL sees them, families in byte order, with Sum, Min and Max families
// typed INT64. Then it lists the instance's materialized views.
func (s *server) sqlTables(instance string) []sqlengine.Table {
	prefix := instance + "/tables/"
	s.mu.Lock()
	byName := map[string]*table{}
	for name, tbl := range s.tables {
		if id, ok := strings.CutPrefix(name, prefix); ok {
			byName[id] = tbl
		}
	}
	s.mu.Unlock()
	var out []sqlengine.Table
	for name, tbl := range byName {
		t := sqlengine.Table{Name: name}
		for fam, cf := range tbl.columnFamilies() {
			f := sqlengine.Family{Name: fam}
			switch cf.valueType.GetAggregateType().GetAggregator().(type) {
			case *btapb.Type_Aggregate_Sum_, *btapb.Type_Aggregate_Min_, *btapb.Type_Aggregate_Max_:
				f.Int64 = true
			}
			t.Families = append(t.Families, f)
		}
		slices.SortFunc(t.Families, func(a, b sqlengine.Family) int { return strings.Compare(a.Name, b.Name) })
		out = append(out, t)
	}
	return append(out, s.sqlViews(instance)...)
}

func (s *server) ExecuteQuery(req *btpb.ExecuteQueryRequest, stream btpb.Bigtable_ExecuteQueryServer) error {
	pq, err := s.preparedQuery(req.GetPreparedQuery())
	if err != nil {
		return err
	}
	params, err := queryParams(pq, req.Params)
	if err != nil {
		return err
	}
	w := &resultWriter{stream: stream}
	src := &tableSource{s: s, instance: pq.instance}
	if err := pq.query.Run(stream.Context(), src, params, func(row []sqlengine.Value) error {
		values := make([]*btpb.Value, len(row))
		for i, v := range row {
			values[i] = v.Proto(pq.query.Columns[i].Type)
		}
		return w.row(values)
	}); err != nil {
		return err
	}
	return w.close()
}

// preparedQuery finds a prepared query and fails as production does once it has expired: 40 s after prepare, or
// after a family it reads is dropped.
func (s *server) preparedQuery(token []byte) (*preparedQuery, error) {
	s.sql.mu.Lock()
	pq := s.sql.queries[string(token)]
	now := s.sql.clock()
	s.sql.mu.Unlock()
	if pq == nil {
		if prepared, ok := preparedAt(token); ok && expired(prepared, now) {
			return nil, expiredError(expiredMessage)
		}
		return nil, status.Error(codes.InvalidArgument, "Invalid prepared query. Please retry request with a valid prepared query")
	}
	if expired(pq.prepared, now) {
		return nil, expiredError(expiredMessage)
	}
	if s.familyDropped(pq) {
		// Production repeats the message for a dropped family.
		return nil, expiredError(expiredMessage + " : " + expiredMessage)
	}
	return pq, nil
}

// familyDropped reports whether a family the query reads has been dropped since prepare. Once it has, it stays
// true.
func (s *server) familyDropped(pq *preparedQuery) bool {
	s.sql.mu.Lock()
	dropped := pq.familyDropped
	s.sql.mu.Unlock()
	if dropped {
		return true
	}
	tbl := s.instanceTable(pq.instance, pq.query.Table)
	if tbl == nil {
		return false
	}
	fams := tbl.columnFamilies()
	for _, f := range pq.query.Families {
		if _, ok := fams[f]; !ok {
			s.sql.mu.Lock()
			pq.familyDropped = true
			s.sql.mu.Unlock()
			return true
		}
	}
	return false
}

// instanceTable returns the instance's table with the given ID, or nil.
func (s *server) instanceTable(instance, id string) *table {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tables[instance+"/tables/"+id]
}

func expiredError(msg string) error {
	st, err := status.New(codes.FailedPrecondition, msg).WithDetails(&errdetails.PreconditionFailure{
		Violations: []*errdetails.PreconditionFailure_Violation{{Type: "PREPARED_QUERY_EXPIRED"}},
	})
	if err != nil {
		return status.Errorf(codes.Internal, "build the expired query error: %v", err)
	}
	return st.Err()
}

// queryParams checks each declared parameter's value and type, with production's messages. It keys the values by
// the lowercased name, the name the analyzer gives a parameter.
func queryParams(pq *preparedQuery, values map[string]*btpb.Value) (map[string]sqlengine.Value, error) {
	out := map[string]sqlengine.Value{}
	for name, t := range pq.paramTypes {
		v, ok := values[name]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "params does not contain key '%s'", name)
		}
		if v.GetType() == nil {
			return nil, status.Errorf(codes.InvalidArgument, "params '%s' has no type", name)
		}
		if !proto.Equal(v.GetType(), t) {
			return nil, status.Errorf(codes.InvalidArgument, "params '%s' has type %s but expected type %s", name, sqlengine.TypeName(v.GetType()), sqlengine.TypeName(t))
		}
		out[strings.ToLower(name)] = sqlengine.ValueFromProto(v, pq.params[name])
	}
	return out, nil
}

// batchBytes is the batch_data size at which production closes a batch. The row that brings a batch to this size
// or more is the batch's last row.
const batchBytes = 2 << 20

// resultWriter frames a result as production does. Each batch is one message with its CRC32C, and only the first
// message has reset set. A message with only the resume token ends the stream. With no rows, the first message holds
// an empty batch and no checksum.
type resultWriter struct {
	stream btpb.Bigtable_ExecuteQueryServer
	data   []byte
	sent   bool
}

// row adds a row to the open batch, and sends the batch once it holds batchBytes or more.
func (w *resultWriter) row(values []*btpb.Value) error {
	// The encodings of two ProtoRows messages, concatenated, decode as one ProtoRows that holds the values of both.
	data, err := proto.MarshalOptions{}.MarshalAppend(w.data, &btpb.ProtoRows{Values: values})
	if err != nil {
		return status.Errorf(codes.Internal, "encode rows: %v", err)
	}
	w.data = data
	if len(w.data) >= batchBytes {
		return w.send()
	}
	return nil
}

func (w *resultWriter) send() error {
	batch := &btpb.PartialResultSet{Reset_: !w.sent, PartialRows: &btpb.PartialResultSet_ProtoRowsBatch{ProtoRowsBatch: &btpb.ProtoRowsBatch{BatchData: w.data}}}
	if len(w.data) > 0 {
		sum := crc32.Checksum(w.data, castagnoli)
		batch.BatchChecksum = &sum
	}
	w.sent = true
	w.data = nil
	return w.stream.Send(&btpb.ExecuteQueryResponse{Response: &btpb.ExecuteQueryResponse_Results{Results: batch}})
}

// close sends the open batch, if it holds a row or no message has gone out, and then the resume token.
func (w *resultWriter) close() error {
	if len(w.data) > 0 || !w.sent {
		if err := w.send(); err != nil {
			return err
		}
	}
	token := &btpb.PartialResultSet{ResumeToken: resumeToken}
	return w.stream.Send(&btpb.ExecuteQueryResponse{Response: &btpb.ExecuteQueryResponse_Results{Results: token}})
}

// tableSource serves SQL rows from a table's btree. It holds the table's read lock only to collect the rows, then
// locks one row at a time to copy its newest cells, as ReadRows does. A query sees each row atomically. It does
// not see a snapshot across rows.
type tableSource struct {
	s        *server
	instance string
}

func (src *tableSource) Scan(ctx context.Context, name string, fn func(sqlengine.Row) error) error {
	tbl := src.s.instanceTable(src.instance, name)
	if tbl == nil {
		return status.Errorf(codes.NotFound, "table %q not found", name)
	}
	var rows []*row
	tbl.mu.RLock()
	tbl.rows.Ascend(func(i btree.Item) bool {
		rows = append(rows, i.(*row))
		return true
	})
	tbl.mu.RUnlock()
	for _, r := range rows {
		r.mu.Lock()
		cells := map[string]map[string][]byte{}
		for fname, fam := range r.families {
			for col, cs := range fam.cells {
				if len(cs) == 0 {
					continue
				}
				if cells[fname] == nil {
					cells[fname] = map[string][]byte{}
				}
				cells[fname][col] = cs[0].value
			}
		}
		r.mu.Unlock()
		if len(cells) == 0 {
			continue
		}
		if err := fn(sqlengine.Row{Key: []byte(r.key), Cells: cells}); err != nil {
			return err
		}
	}
	return nil
}
