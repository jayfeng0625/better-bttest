// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The rows and tables of any run, so that a cleanup also removes what an interrupted run left.
var (
	caseRowKey  = regexp.MustCompile(`^` + rowPrefix + `[0-9a-f]{12}#`)
	caseTableID = regexp.MustCompile(`^` + tablePrefix + `[0-9a-f]{12}-t[0-9]+$`)
)

// Cleanup deletes every row that the cases wrote to the parity table and every table they created, from any run.
// It then reads both again, and returns an error naming any that remain.
func Cleanup(ctx context.Context, t Target) error {
	keys, err := caseRows(ctx, t)
	if err != nil {
		return err
	}
	var errs []error
	if len(keys) > 0 {
		errs = append(errs, deleteRows(ctx, t, keys))
	}
	tables, err := caseTables(ctx, t)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, id := range tables {
		if err := t.deleteTable(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}

	leftRows, err := caseRows(ctx, t)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	leftTables, err := caseTables(ctx, t)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if len(leftRows)+len(leftTables) > 0 {
		errs = append(errs, fmt.Errorf("left after cleanup: %d rows and the tables %v", len(leftRows), leftTables))
	}
	return errors.Join(errs...)
}

func caseRows(ctx context.Context, t Target) ([][]byte, error) {
	stream, err := t.Data.ReadRows(ctx, &btpb.ReadRowsRequest{
		TableName: t.tablePath(parityTable),
		Rows: &btpb.RowSet{RowRanges: []*btpb.RowRange{{
			StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte(rowPrefix)},
			EndKey:   &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte(strings.TrimSuffix(rowPrefix, "#") + "$")},
		}}},
		Filter: &btpb.RowFilter{Filter: &btpb.RowFilter_Chain_{Chain: &btpb.RowFilter_Chain{Filters: []*btpb.RowFilter{
			{Filter: &btpb.RowFilter_CellsPerRowLimitFilter{CellsPerRowLimitFilter: 1}},
			{Filter: &btpb.RowFilter_StripValueTransformer{StripValueTransformer: true}},
		}}}},
	})
	if err != nil {
		return nil, err
	}
	var keys [][]byte
	err = recvAll(stream, func(resp *btpb.ReadRowsResponse) {
		for _, ch := range resp.Chunks {
			if caseRowKey.Match(ch.RowKey) {
				keys = append(keys, ch.RowKey)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func deleteRows(ctx context.Context, t Target, keys [][]byte) error {
	req := &btpb.MutateRowsRequest{TableName: t.tablePath(parityTable)}
	for _, key := range keys {
		req.Entries = append(req.Entries, &btpb.MutateRowsRequest_Entry{RowKey: key, Mutations: Mutations(DeleteFromRow())})
	}
	stream, err := t.Data.MutateRows(ctx, req)
	if err != nil {
		return err
	}
	var errs []error
	err = recvAll(stream, func(resp *btpb.MutateRowsResponse) {
		for _, e := range resp.Entries {
			if codes.Code(e.Status.GetCode()) != codes.OK {
				errs = append(errs, fmt.Errorf("delete row %q: %w", keys[e.Index], status.ErrorProto(e.Status)))
			}
		}
	})
	return errors.Join(append(errs, err)...)
}

func caseTables(ctx context.Context, t Target) ([]string, error) {
	req := &adminpb.ListTablesRequest{Parent: t.Instance, View: adminpb.Table_NAME_ONLY}
	var ids []string
	for {
		resp, err := t.Admin.ListTables(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, tbl := range resp.Tables {
			if id := strings.TrimPrefix(tbl.Name, t.tablePath("")); caseTableID.MatchString(id) {
				ids = append(ids, id)
			}
		}
		if resp.NextPageToken == "" {
			return ids, nil
		}
		req.PageToken = resp.NextPageToken
	}
}

// Delete the table, clearing deletion protection first, since a case that stops partway can leave its table protected.
// A table that is not found is already deleted.
func (t Target) deleteTable(ctx context.Context, id string) error {
	path := t.tablePath(id)
	if err := t.updateTable(ctx, &adminpb.Table{Name: path}, "deletion_protection", false); status.Code(err) == codes.NotFound {
		return nil
	} else if err != nil {
		return fmt.Errorf("clear deletion protection on %s: %w", id, err)
	}
	if _, err := t.Admin.DeleteTable(ctx, &adminpb.DeleteTableRequest{Name: path}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	return nil
}
