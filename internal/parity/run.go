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
// the run, which names its tables, so every target gives a case the same table names. Run deletes the case's tables
// before it returns.
func Run(ctx context.Context, t Target, runID string, ordinal int, c Case) (results []Result, err error) {
	r := &runner{
		target:       t,
		rowKeyPrefix: rowPrefix + runID + "#" + c.Name,
		tablePrefix:  fmt.Sprintf("%s%s-t%d", tablePrefix, runID, ordinal),
		tables:       map[string]string{},
	}
	defer func() { err = errors.Join(err, r.deleteTables(ctx)) }()
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
	tablePrefix  string
	// Each table label's id, and the labels in the order the case first named them.
	tables map[string]string
	labels []string
}

// The id of the case's table with the label. The first label gets the run's name for the case, and each later label
// adds its place.
func (r *runner) tableID(label string) string {
	if id, ok := r.tables[label]; ok {
		return id
	}
	id := r.tablePrefix
	if n := len(r.labels); n > 0 {
		id += fmt.Sprintf("-%d", n+1)
	}
	r.tables[label] = id
	r.labels = append(r.labels, label)
	return id
}

func (r *runner) tablePath(label string) string { return r.target.tablePath(r.tableID(label)) }

// Where a data call goes: the case's row on the parity table when it names no table, or the exact key on a case's
// table when it does.
func (r *runner) rowOn(table string, row Row) (tableName string, key []byte) {
	if table != "" {
		return r.tablePath(table), []byte(row)
	}
	if row == "" {
		return r.target.tablePath(parityTable), []byte(r.rowKeyPrefix)
	}
	return r.target.tablePath(parityTable), []byte(r.rowKeyPrefix + "#" + string(row))
}

// Delete the case's tables, clearing deletion protection first. A table the case never created is not found.
func (r *runner) deleteTables(ctx context.Context) error {
	var errs []error
	for _, label := range r.labels {
		path := r.target.tablePath(r.tables[label])
		if err := r.target.updateTable(ctx, &adminpb.Table{Name: path}, "deletion_protection", false); status.Code(err) == codes.NotFound {
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("clear deletion protection on %s: %w", path, err))
		}
		if _, err := r.target.Admin.DeleteTable(ctx, &adminpb.DeleteTableRequest{Name: path}); err != nil && status.Code(err) != codes.NotFound {
			errs = append(errs, fmt.Errorf("delete %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
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
		table, key := r.rowOn(c.Table, c.Row)
		_, err = r.target.Data.MutateRow(ctx, &btpb.MutateRowRequest{TableName: table, RowKey: key, Mutations: c.Mutations})
	case MutateRows:
		res.Entries, err = r.mutateRows(ctx, c.Entries)
	case CheckAndMutate:
		table, key := r.rowOn("", c.Row)
		var resp *btpb.CheckAndMutateRowResponse
		resp, err = r.target.Data.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{
			TableName: table, RowKey: key, TrueMutations: c.True, FalseMutations: c.False,
		})
		if err == nil {
			res.Matched = &resp.PredicateMatched
		}
	case ReadModifyWrite:
		table, key := r.rowOn("", c.Row)
		_, err = r.target.Data.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{TableName: table, RowKey: key, Rules: c.Rules})
	case ReadRow:
		res.Cells, err = r.readRow(ctx, c.Table, c.Row)
	case ReadRowKeys:
		res.Keys, err = r.readRowKeys(ctx, c.Table)
	case CreateTable:
		_, err = r.target.Admin.CreateTable(ctx, &adminpb.CreateTableRequest{
			Parent: r.target.Instance, TableId: r.tableID(c.Table),
			Table: &adminpb.Table{
				ColumnFamilies: map[string]*adminpb.ColumnFamily{"cf": {GcRule: &adminpb.GcRule{Rule: &adminpb.GcRule_MaxNumVersions{MaxNumVersions: 1}}}},
				RowKeySchema:   c.Schema,
			},
		})
	case SetRowKeySchema:
		err = r.target.updateTable(ctx, &adminpb.Table{Name: r.tablePath(c.Table), RowKeySchema: c.Schema}, "row_key_schema", c.IgnoreWarnings)
	case SetDeletionProtection:
		err = r.target.updateTable(ctx, &adminpb.Table{Name: r.tablePath(c.Table), DeletionProtection: c.On}, "deletion_protection", false)
	case GetTable:
		var tbl *adminpb.Table
		tbl, err = r.target.Admin.GetTable(ctx, &adminpb.GetTableRequest{Name: r.tablePath(c.Table), View: adminpb.Table_SCHEMA_VIEW})
		if err == nil {
			res.Table = &TableView{RowKeySchema: tbl.RowKeySchema, DeletionProtection: tbl.DeletionProtection}
		}
		// Production's NotFound message differs from upstream's.
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
		req.TableName, key = r.rowOn("", e.Row)
		req.Entries = append(req.Entries, &btpb.MutateRowsRequest_Entry{RowKey: key, Mutations: e.Mutations})
	}
	stream, err := r.target.Data.MutateRows(ctx, req)
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, len(entries))
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return statuses, nil
		}
		if err != nil {
			return nil, err
		}
		for _, e := range resp.Entries {
			statuses[e.Index] = Status{Code: codes.Code(e.Status.GetCode()), Message: e.Status.GetMessage()}
		}
	}
}

// The row's cells as raw bytes. A chunk with a value_size continues in the next chunk, and any other chunk ends its
// cell. A chunk that names no family or qualifier keeps the previous cell's.
func (r *runner) readRow(ctx context.Context, table string, row Row) ([]Cell, error) {
	tableName, key := r.rowOn(table, row)
	stream, err := r.target.Data.ReadRows(ctx, &btpb.ReadRowsRequest{TableName: tableName, Rows: &btpb.RowSet{RowKeys: [][]byte{key}}})
	if err != nil {
		return nil, err
	}
	var cells []Cell
	var family, qualifier string
	continuing := false
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return cells, nil
		}
		if err != nil {
			return nil, err
		}
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
	}
}

// The key of each row in the case's table, in the order ReadRows returns them. A chunk names its row only when the row
// starts, and a row that the server resets after a reset_row starts again with the same key.
func (r *runner) readRowKeys(ctx context.Context, table string) ([]Hex, error) {
	stream, err := r.target.Data.ReadRows(ctx, &btpb.ReadRowsRequest{TableName: r.tablePath(table)})
	if err != nil {
		return nil, err
	}
	var keys []Hex
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return keys, nil
		}
		if err != nil {
			return nil, err
		}
		for _, ch := range resp.Chunks {
			if len(ch.RowKey) > 0 && (len(keys) == 0 || string(keys[len(keys)-1]) != string(ch.RowKey)) {
				keys = append(keys, ch.RowKey)
			}
		}
	}
}
