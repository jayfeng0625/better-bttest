// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"slices"
	"strings"
	"sync"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/btree"
	"github.com/jayfeng0625/better-bttest/bttest/internal/sqlengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// materializedViews holds the server's materialized views, keyed by their full names. A view stores its query as
// create compiled it, and each SQL read of the view evaluates it.
type materializedViews struct {
	mu    sync.Mutex
	views map[string]*materializedView
}

// materializedView is a stored view, its compiled query, and the full name of the table its query reads. A build
// without SQL compiles no query and leaves the table empty.
type materializedView struct {
	mv    *btapb.MaterializedView
	view  *sqlengine.View
	table string
}

// viewReferences fails a table's deletion while a view reads it, with production's message.
func (s *server) viewReferences(table string) error {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	var refs []string
	for name, v := range s.views.views {
		if v.table == table {
			refs = append(refs, name)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	slices.Sort(refs)
	return status.Errorf(codes.FailedPrecondition, "Unable to delete resource %s because the resource is referenced by another resource. The existing references are: {%s}", table, strings.Join(refs, ", "))
}

// readRowsTable returns the table that a ReadRows reads: the request's materialized view, evaluated, when it names
// one, and its table otherwise.
func (s *server) readRowsTable(ctx context.Context, req *btpb.ReadRowsRequest) (*table, error) {
	if req.MaterializedViewName != "" {
		return s.viewTable(ctx, req.MaterializedViewName)
	}
	s.mu.Lock()
	tbl, ok := s.tables[req.TableName]
	s.mu.Unlock()
	if !ok {
		return nil, tableNotFound(req.TableName)
	}
	return tbl, nil
}

// viewTable evaluates the view into a table as production's ReadRows shows a view: one family, default, where each
// row has an empty-qualifier cell with an empty value, then a cell per stored column, all at timestamp 0.
func (s *server) viewTable(ctx context.Context, name string) (*table, error) {
	s.views.mu.Lock()
	v, ok := s.views.views[name]
	s.views.mu.Unlock()
	if !ok {
		return nil, tableNotFound(name)
	}
	instance, _, _ := strings.Cut(name, "/materializedViews/")
	q, err := sqlengine.CurrentView(v.view, s.sqlTables(instance))
	if err != nil {
		return nil, err
	}
	tbl := &table{rows: btree.New(btreeDegree)}
	err = q.ViewRows(ctx, &tableSource{s: s, instance: instance}, func(key []byte, cells []sqlengine.ViewCell) error {
		r := newRow(string(key))
		fam := r.getOrCreateFamily("default", 0)
		for _, c := range append([]sqlengine.ViewCell{{}}, cells...) {
			fam.cells[c.Column] = append(fam.cellsByColumn(c.Column), cell{value: c.Value})
		}
		tbl.rows.ReplaceOrInsert(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tbl, nil
}

func (s *server) sqlViews(instance string) []sqlengine.Table {
	prefix := instance + "/materializedViews/"
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	var out []sqlengine.Table
	for name, v := range s.views.views {
		if id, ok := strings.CutPrefix(name, prefix); ok {
			out = append(out, sqlengine.Table{Name: id, View: v.view})
		}
	}
	return out
}

// CreateMaterializedView checks the query with CheckView, then stores the view. It returns a finished operation,
// so the Go client's Wait makes no GetOperation call.
func (s *server) CreateMaterializedView(ctx context.Context, req *btapb.CreateMaterializedViewRequest) (*longrunning.Operation, error) {
	query := req.GetMaterializedView().GetQuery()
	view, err := sqlengine.CheckView(query, s.sqlTables(req.GetParent()))
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/materializedViews/" + req.GetMaterializedViewId()
	mv := &btapb.MaterializedView{
		Name:               name,
		Query:              query,
		DeletionProtection: req.GetMaterializedView().GetDeletionProtection(),
	}
	v := &materializedView{mv: mv, view: view}
	if view != nil {
		v.table = req.GetParent() + "/tables/" + view.Table()
	}
	// CheckView ran without a lock, so a DeleteTable may have removed the table since. Holding s.mu until the view
	// is stored makes the check and the store one step against DeleteTable, which takes s.mu then s.views.mu too.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tables[v.table]; v.table != "" && !ok {
		return nil, tableNotFound(v.table)
	}
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if _, ok := s.views.views[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Materialized View %s already exists.", req.GetMaterializedViewId())
	}
	if s.views.views == nil {
		s.views.views = map[string]*materializedView{}
	}
	s.views.views[name] = v
	return doneOperation(mv)
}

func doneOperation(mv *btapb.MaterializedView) (*longrunning.Operation, error) {
	res, err := anypb.New(mv)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode the operation response: %v", err)
	}
	return &longrunning.Operation{Name: mv.Name + "/operations/done", Done: true, Result: &longrunning.Operation_Response{Response: res}}, nil
}

func (s *server) GetMaterializedView(ctx context.Context, req *btapb.GetMaterializedViewRequest) (*btapb.MaterializedView, error) {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	v, ok := s.views.views[req.GetName()]
	if !ok {
		return nil, readNotFound(req.GetName())
	}
	return proto.Clone(v.mv).(*btapb.MaterializedView), nil
}

// viewReplica fails a read of a deleted view as production does when the view's replica is gone. The emulator has
// no cluster, so it names one after the instance.
func (s *server) viewReplica(instance, id string) error {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if _, ok := s.views.views[instance+"/materializedViews/"+id]; ok {
		return nil
	}
	project, instanceID, ok := projectInstance(instance)
	if !ok {
		return status.Errorf(codes.NotFound, "Failed to read: %s/materializedViews/%s", instance, id)
	}
	cluster := instanceID + "-c1"
	replica := numberedProject(instance) + "/clusters/" + cluster + "/materializedViews/" + id
	return status.Errorf(codes.NotFound, "Failed to read: %s : MaterializedViewsReplicas(%s,%s,%s,%s) : Failed to read: %s", replica, project, instanceID, cluster, id, replica)
}

func projectInstance(instance string) (project, id string, ok bool) {
	parts := strings.Split(instance, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "instances" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// ListMaterializedViews returns the instance's views in name order, in one page.
func (s *server) ListMaterializedViews(ctx context.Context, req *btapb.ListMaterializedViewsRequest) (*btapb.ListMaterializedViewsResponse, error) {
	prefix := req.GetParent() + "/materializedViews/"
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	res := &btapb.ListMaterializedViewsResponse{}
	for name, v := range s.views.views {
		if strings.HasPrefix(name, prefix) {
			res.MaterializedViews = append(res.MaterializedViews, proto.Clone(v.mv).(*btapb.MaterializedView))
		}
	}
	slices.SortFunc(res.MaterializedViews, func(a, b *btapb.MaterializedView) int { return strings.Compare(a.Name, b.Name) })
	return res, nil
}

func (s *server) DeleteMaterializedView(ctx context.Context, req *btapb.DeleteMaterializedViewRequest) (*emptypb.Empty, error) {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	v, ok := s.views.views[req.GetName()]
	if !ok {
		return nil, readNotFound(req.GetName())
	}
	if v.mv.DeletionProtection {
		return nil, status.Errorf(codes.FailedPrecondition, "Materialized View %s has deletion protection enabled.", req.GetName())
	}
	delete(s.views.views, req.GetName())
	return &emptypb.Empty{}, nil
}

// UpdateMaterializedView changes deletion protection. An update mask that names the query must carry the stored
// query.
func (s *server) UpdateMaterializedView(ctx context.Context, req *btapb.UpdateMaterializedViewRequest) (*longrunning.Operation, error) {
	in := req.GetMaterializedView()
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	v, ok := s.views.views[in.GetName()]
	if !ok {
		return nil, readNotFound(in.GetName())
	}
	mv := v.mv
	paths := req.GetUpdateMask().GetPaths()
	if slices.Contains(paths, "query") && in.GetQuery() != mv.Query {
		return nil, status.Error(codes.InvalidArgument, "Immutable fields 'query,name' cannot be updated.")
	}
	if slices.Contains(paths, "deletion_protection") {
		mv.DeletionProtection = in.GetDeletionProtection()
	}
	return doneOperation(mv)
}
