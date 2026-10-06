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

func (r *runner) call(ctx context.Context, call Call) Result {
	res, err := call.run(ctx, r)
	res.Call = reflect.TypeOf(call).Name()
	s := status.Convert(err)
	res.Status = Status{Code: s.Code(), Message: s.Message()}
	return res
}

func (r *runner) tablePath() string { return r.target.tablePath(r.table) }

// The path of a data call's table.
func (r *runner) dataTable(caseTable bool) string {
	if caseTable {
		return r.tablePath()
	}
	return r.target.tablePath(parityTable)
}

// The key of a data call's row.
func (r *runner) rowKey(caseTable bool, row Row) []byte {
	switch {
	case caseTable:
		return []byte(row)
	case row == "":
		return []byte(r.rowKeyPrefix)
	}
	return []byte(r.rowKeyPrefix + "#" + string(row))
}

func (c MutateRow) run(ctx context.Context, r *runner) (Result, error) {
	_, err := r.target.Data.MutateRow(ctx, &btpb.MutateRowRequest{
		TableName: r.dataTable(c.CaseTable), RowKey: r.rowKey(c.CaseTable, c.Row), Mutations: c.Mutations,
	})
	return Result{}, err
}

// Each entry's status, in entry order. The stream can return the entries in any order.
func (c MutateRows) run(ctx context.Context, r *runner) (Result, error) {
	req := &btpb.MutateRowsRequest{TableName: r.dataTable(c.CaseTable)}
	for _, e := range c.Entries {
		req.Entries = append(req.Entries, &btpb.MutateRowsRequest_Entry{RowKey: r.rowKey(c.CaseTable, e.Row), Mutations: e.Mutations})
	}
	stream, err := r.target.Data.MutateRows(ctx, req)
	if err != nil {
		return Result{}, err
	}
	entries := make([]Status, len(c.Entries))
	err = recvAll(stream, func(resp *btpb.MutateRowsResponse) {
		for _, e := range resp.Entries {
			entries[e.Index] = Status{Code: codes.Code(e.Status.GetCode()), Message: e.Status.GetMessage()}
		}
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Entries: entries}, nil
}

func (c CheckAndMutate) run(ctx context.Context, r *runner) (Result, error) {
	resp, err := r.target.Data.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{
		TableName: r.dataTable(c.CaseTable), RowKey: r.rowKey(c.CaseTable, c.Row), TrueMutations: c.True, FalseMutations: c.False,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Matched: &resp.PredicateMatched}, nil
}

func (c ReadModifyWrite) run(ctx context.Context, r *runner) (Result, error) {
	_, err := r.target.Data.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{
		TableName: r.dataTable(c.CaseTable), RowKey: r.rowKey(c.CaseTable, c.Row), Rules: c.Rules,
	})
	return Result{}, err
}

// The row's cells as raw bytes. A chunk with a value_size continues in the next chunk, and any other chunk ends its
// cell. A chunk that names no family or qualifier keeps the previous cell's.
func (c ReadRow) run(ctx context.Context, r *runner) (Result, error) {
	stream, err := r.target.Data.ReadRows(ctx, &btpb.ReadRowsRequest{
		TableName: r.dataTable(c.CaseTable), Rows: &btpb.RowSet{RowKeys: [][]byte{r.rowKey(c.CaseTable, c.Row)}},
	})
	if err != nil {
		return Result{}, err
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
		return Result{}, err
	}
	return Result{Cells: cells}, nil
}

func (ReadRowKeys) run(ctx context.Context, r *runner) (Result, error) {
	keys, err := readKeys(ctx, r.target, &btpb.ReadRowsRequest{TableName: r.tablePath()})
	return Result{Keys: keys}, err
}

// The key of each row that the request reads, in order. A chunk names its row only when the row starts, and a row that
// the server resets after a reset_row starts again with the same key.
func readKeys(ctx context.Context, t Target, req *btpb.ReadRowsRequest) ([]Hex, error) {
	stream, err := t.Data.ReadRows(ctx, req)
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

func (SampleRowKeys) run(ctx context.Context, r *runner) (Result, error) {
	stream, err := r.target.Data.SampleRowKeys(ctx, &btpb.SampleRowKeysRequest{TableName: r.tablePath()})
	if err != nil {
		return Result{}, err
	}
	return Result{}, recvAll(stream, func(*btpb.SampleRowKeysResponse) {})
}

func (c CreateTable) run(ctx context.Context, r *runner) (Result, error) {
	r.created = true
	families := c.Families
	if families == nil {
		families = map[string]*adminpb.ColumnFamily{"cf": {GcRule: &adminpb.GcRule{Rule: &adminpb.GcRule_MaxNumVersions{MaxNumVersions: 1}}}}
	}
	_, err := r.target.Admin.CreateTable(ctx, &adminpb.CreateTableRequest{
		Parent: r.target.Instance, TableId: r.table,
		Table: &adminpb.Table{ColumnFamilies: families, RowKeySchema: c.Schema},
	})
	return Result{}, err
}

func (c SetRowKeySchema) run(ctx context.Context, r *runner) (Result, error) {
	return Result{}, r.target.updateTable(ctx, &adminpb.Table{Name: r.tablePath(), RowKeySchema: c.Schema}, "row_key_schema", c.IgnoreWarnings)
}

func (c SetDeletionProtection) run(ctx context.Context, r *runner) (Result, error) {
	return Result{}, r.target.updateTable(ctx, &adminpb.Table{Name: r.tablePath(), DeletionProtection: c.On}, "deletion_protection", false)
}

func (c ModifyColumnFamilies) run(ctx context.Context, r *runner) (Result, error) {
	mods := c.Mods
	if mods == nil {
		mods = []*adminpb.ModifyColumnFamiliesRequest_Modification{CreateFamily("cf2", &adminpb.ColumnFamily{})}
	}
	_, err := r.target.Admin.ModifyColumnFamilies(ctx, &adminpb.ModifyColumnFamiliesRequest{Name: r.tablePath(), Modifications: mods})
	return Result{}, err
}

func (c DropRowRange) run(ctx context.Context, r *runner) (Result, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Deadline)
	defer cancel()
	_, err := r.target.Admin.DropRowRange(ctx, &adminpb.DropRowRangeRequest{
		Name: r.tablePath(), Target: &adminpb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: true},
	})
	return Result{}, err
}

func (DeleteTable) run(ctx context.Context, r *runner) (Result, error) {
	_, err := r.target.Admin.DeleteTable(ctx, &adminpb.DeleteTableRequest{Name: r.tablePath()})
	return Result{}, err
}

func (GenerateConsistencyToken) run(ctx context.Context, r *runner) (Result, error) {
	_, err := r.target.Admin.GenerateConsistencyToken(ctx, &adminpb.GenerateConsistencyTokenRequest{Name: r.tablePath()})
	return Result{}, err
}

func (CheckConsistency) run(ctx context.Context, r *runner) (Result, error) {
	_, err := r.target.Admin.CheckConsistency(ctx, &adminpb.CheckConsistencyRequest{Name: r.tablePath(), ConsistencyToken: "token"})
	return Result{}, err
}

func (GetTable) run(ctx context.Context, r *runner) (Result, error) {
	tbl, err := r.target.Admin.GetTable(ctx, &adminpb.GetTableRequest{Name: r.tablePath(), View: adminpb.Table_SCHEMA_VIEW})
	if err != nil {
		return Result{}, err
	}
	return Result{Table: &TableView{
		ColumnFamilies: tbl.ColumnFamilies, RowKeySchema: tbl.RowKeySchema, DeletionProtection: tbl.DeletionProtection,
	}}, nil
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
