// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Every row the cases write on the parity table starts with this prefix and the run id.
const rowPrefix = "probe#"

// Every table the cases create starts with this prefix and the run id, to fit the 50-character table id limit.
const tablePrefix = parityTable + "-"

// Run makes the case's calls on the target, and returns each of Calls' results. The ordinal is the case's place in
// the run, which names its table, so every target gives a case the same table name. Run deletes the case's table
// before it returns.
func Run(ctx context.Context, t Target, runID string, ordinal int, c Case) (results []Result, err error) {
	r := &runner{
		target:       t,
		rowKeyPrefix: rowPrefix + runID + "#" + c.Name,
		table:        fmt.Sprintf("%s%s-t%d", tablePrefix, runID, ordinal),
	}
	defer func() {
		if r.created {
			err = errors.Join(err, t.deleteTable(ctx, r.table))
		}
	}()
	for i, call := range c.Setup {
		if res := r.call(ctx, call); res.Status.Code != codes.OK {
			return nil, fmt.Errorf("setup call %d %s: %s: %s", i, res.Call, res.Status.Code, res.Status.Message)
		}
	}
	results = make([]Result, len(c.Calls))
	for i, call := range c.Calls {
		results[i] = r.call(ctx, call)
	}
	return results, nil
}

type runner struct {
	target       Target
	rowKeyPrefix string
	table        string // the case's table's id
	created      bool   // whether the case has called CreateTable
}

func (r *runner) tablePath() string { return r.target.tablePath(r.table) }

func (r *runner) rowOn(caseTable bool, row Row) (tableName string, key []byte) {
	if caseTable {
		return r.tablePath(), []byte(row)
	}
	if row == "" {
		return r.target.tablePath(parityTable), []byte(r.rowKeyPrefix)
	}
	return r.target.tablePath(parityTable), []byte(r.rowKeyPrefix + "#" + string(row))
}

func statusOf(err error) Status {
	s := status.Convert(err)
	return Status{Code: s.Code(), Message: s.Message()}
}

func (r *runner) call(ctx context.Context, call Call) Result {
	res := Result{Call: reflect.TypeOf(call).Name()}
	var err error
	switch c := call.(type) {
	case MutateRow:
		table, key := r.rowOn(c.CaseTable, c.Row)
		_, err = r.target.Data.MutateRow(ctx, &btpb.MutateRowRequest{TableName: table, RowKey: key, Mutations: c.Mutations})
	case MutateRows:
		res.Entries, err = r.mutateRows(ctx, c.Entries)
	case CheckAndMutate:
		table, key := r.rowOn(false, c.Row)
		var resp *btpb.CheckAndMutateRowResponse
		resp, err = r.target.Data.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{
			TableName: table, RowKey: key, TrueMutations: c.True, FalseMutations: c.False,
		})
		if err == nil {
			res.Matched = &resp.PredicateMatched
		}
	case ReadModifyWrite:
		table, key := r.rowOn(false, c.Row)
		_, err = r.target.Data.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{TableName: table, RowKey: key, Rules: c.Rules})
	case ReadRow:
		res.Cells, err = r.readRow(ctx, c.CaseTable, c.Row)
	case ReadRowKeys:
		res.Keys, err = r.readRowKeys(ctx)
	case CreateTable:
		r.created = true
		_, err = r.target.Admin.CreateTable(ctx, &adminpb.CreateTableRequest{
			Parent: r.target.Instance, TableId: r.table,
			Table: &adminpb.Table{
				ColumnFamilies: map[string]*adminpb.ColumnFamily{"cf": {GcRule: &adminpb.GcRule{Rule: &adminpb.GcRule_MaxNumVersions{MaxNumVersions: 1}}}},
				RowKeySchema:   c.Schema,
			},
		})
	case SetRowKeySchema:
		err = r.target.updateTable(ctx, &adminpb.Table{Name: r.tablePath(), RowKeySchema: c.Schema}, "row_key_schema", c.IgnoreWarnings)
	case SetDeletionProtection:
		err = r.target.updateTable(ctx, &adminpb.Table{Name: r.tablePath(), DeletionProtection: c.On}, "deletion_protection", false)
	case GetTable:
		var tbl *adminpb.Table
		tbl, err = r.target.Admin.GetTable(ctx, &adminpb.GetTableRequest{Name: r.tablePath(), View: adminpb.Table_SCHEMA_VIEW})
		if err == nil {
			res.Table = &TableView{RowKeySchema: tbl.RowKeySchema, DeletionProtection: tbl.DeletionProtection}
		}
		// Production says "Not found: <table path>", and the emulator says "table \"<table path>\" not found".
		if status.Code(err) == codes.NotFound {
			res.Status = Status{Code: codes.NotFound}
			return res
		}
	default:
		panic(fmt.Sprintf("parity: no run for %T", call))
	}
	res.Status = statusOf(err)
	return res
}

// Each entry's status, in entry order. The stream can return the entries in any order.
func (r *runner) mutateRows(ctx context.Context, entries []Entry) ([]Status, error) {
	req := &btpb.MutateRowsRequest{}
	for _, e := range entries {
		var key []byte
		req.TableName, key = r.rowOn(false, e.Row)
		req.Entries = append(req.Entries, &btpb.MutateRowsRequest_Entry{RowKey: key, Mutations: e.Mutations})
	}
	stream, err := r.target.Data.MutateRows(ctx, req)
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, len(entries))
	err = recvAll(stream, func(resp *btpb.MutateRowsResponse) {
		for _, e := range resp.Entries {
			statuses[e.Index] = Status{Code: codes.Code(e.Status.GetCode()), Message: e.Status.GetMessage()}
		}
	})
	if err != nil {
		return nil, err
	}
	return statuses, nil
}

// The row's cells as raw bytes. A chunk with a value_size continues in the next chunk, and any other chunk ends its
// cell. A chunk that names no family or qualifier keeps the previous cell's.
func (r *runner) readRow(ctx context.Context, caseTable bool, row Row) ([]Cell, error) {
	tableName, key := r.rowOn(caseTable, row)
	stream, err := r.target.Data.ReadRows(ctx, &btpb.ReadRowsRequest{TableName: tableName, Rows: &btpb.RowSet{RowKeys: [][]byte{key}}})
	if err != nil {
		return nil, err
	}
	var cells []Cell
	var family, qualifier string
	continuing := false
	err = recvAll(stream, func(resp *btpb.ReadRowsResponse) {
		for _, ch := range resp.Chunks {
			if ch.GetResetRow() {
				cells, continuing = nil, false
				continue
			}
			if !continuing {
				if ch.FamilyName != nil {
					family = ch.FamilyName.Value
				}
				if ch.Qualifier != nil {
					qualifier = string(ch.Qualifier.Value)
				}
				cells = append(cells, Cell{Column: family + ":" + qualifier, TS: ch.TimestampMicros})
			}
			last := &cells[len(cells)-1]
			last.Value = append(last.Value, ch.Value...)
			continuing = ch.ValueSize > 0
		}
	})
	if err != nil {
		return nil, err
	}
	return cells, nil
}

// The key of each row in the case's table, in the order ReadRows returns them. A chunk names its row only when the row
// starts, and a row that the server resets after a reset_row starts again with the same key.
func (r *runner) readRowKeys(ctx context.Context) ([]Hex, error) {
	stream, err := r.target.Data.ReadRows(ctx, &btpb.ReadRowsRequest{TableName: r.tablePath()})
	if err != nil {
		return nil, err
	}
	var keys []Hex
	err = recvAll(stream, func(resp *btpb.ReadRowsResponse) {
		for _, ch := range resp.Chunks {
			if len(ch.RowKey) > 0 && (len(keys) == 0 || string(keys[len(keys)-1]) != string(ch.RowKey)) {
				keys = append(keys, ch.RowKey)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// Pass each response on the stream to f, until the stream ends.
func recvAll[T any](stream interface{ Recv() (T, error) }, f func(T)) error {
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		f(resp)
	}
}
