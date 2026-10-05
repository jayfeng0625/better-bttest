// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
)

// A Result is one call's outcome. Only the run builds a Result, and the call's type sets which fields it fills:
//
//	every call       Call, Status
//	MutateRows       Entries
//	CheckAndMutate   Matched
//	ReadRow          Cells
//	ReadRowKeys      Keys
//	GetTable         Table
type Result struct {
	Call    string
	Status  Status
	Entries []Status   `json:",omitempty"`
	Matched *bool      `json:",omitempty"`
	Cells   []Cell     `json:",omitempty"`
	Keys    []Hex      `json:",omitempty"`
	Table   *TableView `json:",omitempty"`
}

// A Status is a gRPC status. The golden files name its code.
type Status struct {
	Code    codes.Code
	Message string `json:",omitempty"`
}

func (s Status) MarshalJSON() ([]byte, error) {
	type named struct {
		Code    string
		Message string `json:",omitempty"`
	}
	return json.Marshal(named{s.Code.String(), s.Message})
}

func (s *Status) UnmarshalJSON(data []byte) error {
	var named struct {
		Code    string
		Message string
	}
	if err := json.Unmarshal(data, &named); err != nil {
		return err
	}
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		if c.String() == named.Code {
			*s = Status{Code: c, Message: named.Message}
			return nil
		}
	}
	return fmt.Errorf("no gRPC code is named %q", named.Code)
}

// A Cell is one stored cell, as raw bytes.
type Cell struct {
	Column string // family:qualifier
	TS     int64
	Value  Hex
}

// Hex is bytes that the golden files write in hex.
type Hex []byte

func (h Hex) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(h)), nil }

func (h *Hex) UnmarshalText(text []byte) (err error) {
	*h, err = hex.DecodeString(string(text))
	return err
}

// A TableView is a table as GetTable with SCHEMA_VIEW returns it. The golden files write the schema as protojson.
type TableView struct {
	RowKeySchema       *adminpb.Type_Struct
	DeletionProtection bool
}

type tableViewJSON struct {
	RowKeySchema       json.RawMessage `json:",omitempty"`
	DeletionProtection bool
}

func (v TableView) MarshalJSON() ([]byte, error) {
	out := tableViewJSON{DeletionProtection: v.DeletionProtection}
	if v.RowKeySchema != nil {
		var err error
		if out.RowKeySchema, err = protojson.Marshal(v.RowKeySchema); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

func (v *TableView) UnmarshalJSON(data []byte) error {
	var in tableViewJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*v = TableView{DeletionProtection: in.DeletionProtection}
	if in.RowKeySchema != nil {
		v.RowKeySchema = &adminpb.Type_Struct{}
		return protojson.Unmarshal(in.RowKeySchema, v.RowKeySchema)
	}
	return nil
}
