// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewRunID returns an id for a run that starts at start: the start as 8 hex digits of Unix seconds, then 4 random hex
// digits. A cleanup reads the start back, to tell an interrupted run from one that is still going.
func NewRunID(start time.Time) string {
	return fmt.Sprintf("%08x%04x", uint32(start.Unix()), rand.N[uint32](1<<16))
}

// A run that started longer ago than this has ended, so its rows and tables are an interrupted run's.
const staleAfter = time.Hour

// The rows and tables of a case, with its run's id.
var (
	caseRowKey  = regexp.MustCompile(`^` + rowPrefix + `([0-9a-f]{12})#`)
	caseTableID = regexp.MustCompile(`^` + tablePrefix + `([0-9a-f]{12})-t[0-9]+$`)
)

// Cleanup deletes the rows that the cases wrote to the parity table and the tables they created, in the run and in
// any run that started over staleAfter ago. It then reads both again, and returns an error naming any that remain.
func Cleanup(ctx context.Context, t Target, runID string) error {
	now := time.Now()
	swept := func(id string) bool {
		start, err := strconv.ParseUint(id[:8], 16, 32)
		return id == runID || err == nil && now.Sub(time.Unix(int64(start), 0)) > staleAfter
	}
	keys, err := caseRows(ctx, t, swept)
	if err != nil {
		return err
	}
	var errs []error
	if len(keys) > 0 {
		errs = append(errs, deleteRows(ctx, t, keys))
	}
	tables, err := caseTables(ctx, t, swept)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, id := range tables {
		errs = append(errs, t.deleteTable(ctx, id))
	}

	leftRows, err := caseRows(ctx, t, swept)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	leftTables, err := caseTables(ctx, t, swept)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if len(leftRows)+len(leftTables) > 0 {
		errs = append(errs, fmt.Errorf("left after cleanup: %d rows and the tables %v", len(leftRows), leftTables))
	}
	return errors.Join(errs...)
}

// The keys of the case rows on the parity table whose run id is of.
func caseRows(ctx context.Context, t Target, of func(runID string) bool) ([]Hex, error) {
	keys, err := readKeys(ctx, t, &btpb.ReadRowsRequest{
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
	return slices.DeleteFunc(keys, func(key Hex) bool {
		m := caseRowKey.FindSubmatch(key)
		return m == nil || !of(string(m[1]))
	}), err
}

func deleteRows(ctx context.Context, t Target, keys []Hex) error {
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

// The ids of the case tables whose run id is of.
func caseTables(ctx context.Context, t Target, of func(runID string) bool) ([]string, error) {
	req := &adminpb.ListTablesRequest{Parent: t.Instance, View: adminpb.Table_NAME_ONLY}
	var ids []string
	for {
		resp, err := t.Admin.ListTables(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, tbl := range resp.Tables {
			id := strings.TrimPrefix(tbl.Name, t.tablePath(""))
			if m := caseTableID.FindStringSubmatch(id); m != nil && of(m[1]) {
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
