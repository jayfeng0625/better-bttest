// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// CreateMaterializedView stores the view without checking its query. It returns a finished operation, so a client
// that waits on the operation makes no GetOperation call.
func (s *server) CreateMaterializedView(ctx context.Context, req *btapb.CreateMaterializedViewRequest) (*longrunning.Operation, error) {
	mv := &btapb.MaterializedView{
		Name:               req.GetParent() + "/materializedViews/" + req.GetMaterializedViewId(),
		Query:              req.GetMaterializedView().GetQuery(),
		DeletionProtection: req.GetMaterializedView().GetDeletionProtection(),
	}
	res, err := anypb.New(mv)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode the operation response: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.views[mv.Name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Materialized View %s already exists.", req.GetMaterializedViewId())
	}
	if s.views == nil {
		s.views = map[string]*btapb.MaterializedView{}
	}
	s.views[mv.Name] = mv
	return &longrunning.Operation{Name: mv.Name + "/operations/done", Done: true, Result: &longrunning.Operation_Response{Response: res}}, nil
}

func (s *server) GetMaterializedView(ctx context.Context, req *btapb.GetMaterializedViewRequest) (*btapb.MaterializedView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.views[req.GetName()]
	if !ok {
		return nil, readNotFound(req.GetName())
	}
	return proto.Clone(v).(*btapb.MaterializedView), nil
}
