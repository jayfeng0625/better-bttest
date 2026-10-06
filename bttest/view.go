// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"slices"
	"strings"
	"sync"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/btree"
	"github.com/jayfeng0625/better-bttest/bttest/internal/sqlengine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// materializedViews holds the server's materialized views, keyed by their full names. A view stores only its
// query, and each SQL read of the view evaluates it.
type materializedViews struct {
	mu    sync.Mutex
	views map[string]*materializedView
}

// materializedView is a stored view and the full name of the table its query reads.
type materializedView struct {
	mv    *btapb.MaterializedView
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
	q, err := sqlengine.PrepareView(v.mv.Query, s.sqlTables(instance))
	if err != nil {
		return nil, err
	}
	tbl := &table{rows: btree.New(btreeDegree)}
	err = q.ViewRows(ctx, &tableSource{s: s, instance: instance}, func(key []byte, cells []sqlengine.ViewCell) error {
		r := newRow(string(key))
		fam := r.getOrCreateFamily("default", 0)
		fam.cellsByColumn("")
		fam.cells[""] = []cell{{value: []byte{}}}
		for _, c := range cells {
			fam.cellsByColumn(c.Column)
			fam.cells[c.Column] = []cell{{value: c.Value}}
		}
		tbl.rows.ReplaceOrInsert(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tbl, nil
}

// sqlViews lists the instance's materialized views as SQL sees them.
func (s *server) sqlViews(instance string) []sqlengine.Table {
	prefix := instance + "/materializedViews/"
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	var out []sqlengine.Table
	for name, v := range s.views.views {
		if id, ok := strings.CutPrefix(name, prefix); ok {
			out = append(out, sqlengine.Table{Name: id, ViewQuery: v.mv.Query})
		}
	}
	return out
}

// CreateMaterializedView checks the query by planning it, then stores the view. It returns a finished operation,
// so the Go client's Wait makes no GetOperation call.
func (s *server) CreateMaterializedView(ctx context.Context, req *btapb.CreateMaterializedViewRequest) (*longrunning.Operation, error) {
	query := req.GetMaterializedView().GetQuery()
	q, err := sqlengine.PrepareView(query, s.sqlTables(req.GetParent()))
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/materializedViews/" + req.GetMaterializedViewId()
	mv := &btapb.MaterializedView{
		Name:               name,
		Query:              query,
		DeletionProtection: req.GetMaterializedView().GetDeletionProtection(),
	}
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if _, ok := s.views.views[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Materialized View %s already exists.", req.GetMaterializedViewId())
	}
	if s.views.views == nil {
		s.views.views = map[string]*materializedView{}
	}
	s.views.views[name] = &materializedView{mv: mv, table: req.GetParent() + "/tables/" + q.Table}
	return doneOperation(mv)
}

// doneOperation returns a finished operation whose response is the view.
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
		return nil, viewNotFound(req.GetName())
	}
	return proto.Clone(v.mv).(*btapb.MaterializedView), nil
}

// viewNotFound is the error for a view that does not exist.
func viewNotFound(name string) error {
	return status.Errorf(codes.NotFound, "Failed to read: %s", numberedProject(name))
}

// viewReplica fails a read of a deleted view as production does when the view's replica is gone. The emulator has
// no cluster, so it names one after the instance.
func (s *server) viewReplica(instance, id string) error {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if _, ok := s.views.views[instance+"/materializedViews/"+id]; ok {
		return nil
	}
	project, instanceID := projectInstance(instance)
	cluster := instanceID + "-c1"
	replica := numberedProject(instance) + "/clusters/" + cluster + "/materializedViews/" + id
	return status.Errorf(codes.NotFound, "Failed to read: %s : MaterializedViewsReplicas(%s,%s,%s,%s) : Failed to read: %s", replica, project, instanceID, cluster, id, replica)
}

// projectInstance splits an instance name into its project and instance IDs.
func projectInstance(instance string) (project, id string) {
	parts := strings.Split(instance, "/")
	return parts[1], parts[3]
}

// numberedProject writes a resource name as production writes it in a read error, with the project in braces.
// Production puts the project number there. The emulator has only the project ID, so it writes the ID.
func numberedProject(name string) string {
	project, rest, _ := strings.Cut(strings.TrimPrefix(name, "projects/"), "/")
	return "projects/{" + project + "}/" + rest
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
		return nil, viewNotFound(req.GetName())
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
		return nil, viewNotFound(in.GetName())
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
