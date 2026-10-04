// SPDX-License-Identifier: Apache-2.0

package bttest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/jayfeng0625/better-bttest/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const (
	schemaProject  = "proj"
	schemaInstance = "instance"
)

var keySchema = bigtable.StructType{
	Fields: []bigtable.StructField{
		{FieldName: "tenantId", FieldType: bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}},
		{FieldName: "partitionId", FieldType: bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}},
		{FieldName: "rowType", FieldType: bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}},
		{FieldName: "rowId", FieldType: bigtable.StringType{Encoding: bigtable.StringUtf8BytesEncoding{}}},
	},
	Encoding: bigtable.StructDelimitedBytesEncoding{Delimiter: []byte("#")},
}

var noFieldsSchema = bigtable.StructType{Encoding: bigtable.StructDelimitedBytesEncoding{Delimiter: []byte("#")}}

type schemaEnv struct {
	conn  *grpc.ClientConn
	admin *bigtable.AdminClient
	data  *bigtable.Client
}

func newSchemaEnv(t *testing.T) *schemaEnv {
	t.Helper()
	ctx := context.Background()
	srv, err := bttest.NewServer("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	admin, err := bigtable.NewAdminClient(ctx, schemaProject, schemaInstance, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	data, err := bigtable.NewClientWithConfig(ctx, schemaProject, schemaInstance,
		bigtable.ClientConfig{MetricsProvider: bigtable.NoopMetricsProvider{}}, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	return &schemaEnv{conn: conn, admin: admin, data: data}
}

func (e *schemaEnv) createTable(t *testing.T, name string, schema *bigtable.StructType) {
	t.Helper()
	conf := &bigtable.TableConf{
		TableID:      name,
		Families:     map[string]bigtable.GCPolicy{"cf": bigtable.NoGcPolicy()},
		RowKeySchema: schema,
	}
	if err := e.admin.CreateTableFromConf(context.Background(), conf); err != nil {
		t.Fatalf("CreateTableFromConf: %v", err)
	}
}

func (e *schemaEnv) wantRowKeySchema(t *testing.T, name string, want *bigtable.StructType) {
	t.Helper()
	info, err := e.admin.TableInfo(context.Background(), name)
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if !reflect.DeepEqual(info.RowKeySchema, want) {
		t.Errorf("row key schema = %+v, want %+v", info.RowKeySchema, want)
	}
}

func TestCreateTableKeepsRowKeySchema(t *testing.T) {
	e := newSchemaEnv(t)
	e.createTable(t, "t", &keySchema)

	e.wantRowKeySchema(t, "t", &keySchema)
}

// updateRowKeySchema sends UpdateTable with mask row_key_schema through the
// generated admin client, which can set ignore_warnings for either a change or
// a clear. A nil schema clears the row key schema.
func (e *schemaEnv) updateRowKeySchema(name string, schema *btapb.Type_Struct, ignoreWarnings bool) error {
	_, err := btapb.NewBigtableTableAdminClient(e.conn).UpdateTable(context.Background(), &btapb.UpdateTableRequest{
		Table: &btapb.Table{
			Name:         "projects/" + schemaProject + "/instances/" + schemaInstance + "/tables/" + name,
			RowKeySchema: schema,
		},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"row_key_schema"}},
		IgnoreWarnings: ignoreWarnings,
	})
	return err
}

// wantInvalidArgument checks the gRPC status in err, which the Go admin
// client wraps.
func wantInvalidArgument(t *testing.T, err error, msg string) {
	t.Helper()
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) || se.GRPCStatus().Code() != codes.InvalidArgument || se.GRPCStatus().Message() != msg {
		t.Errorf("error = %v, want InvalidArgument %q", err, msg)
	}
}

func TestUpdateTableRejectsRowKeySchemaChange(t *testing.T) {
	const msg = "Row key schema in-place modification is not allowed."
	other := &btapb.Type_Struct{
		Fields: []*btapb.Type_Struct_Field{
			{FieldName: "tenantId", Type: &btapb.Type{Kind: &btapb.Type_StringType{StringType: &btapb.Type_String{
				Encoding: &btapb.Type_String_Encoding{Encoding: &btapb.Type_String_Encoding_Utf8Bytes_{}},
			}}}},
		},
		Encoding: &btapb.Type_Struct_Encoding{Encoding: &btapb.Type_Struct_Encoding_DelimitedBytes_{
			DelimitedBytes: &btapb.Type_Struct_Encoding_DelimitedBytes{Delimiter: []byte("#")},
		}},
	}
	otherSchema := keySchema
	otherSchema.Encoding = bigtable.StructDelimitedBytesEncoding{Delimiter: []byte("|")}
	e := newSchemaEnv(t)
	e.createTable(t, "t", &keySchema)

	wantInvalidArgument(t, e.admin.UpdateTableWithRowKeySchema(context.Background(), "t", otherSchema), msg)
	wantInvalidArgument(t, e.admin.UpdateTableWithRowKeySchema(context.Background(), "t", noFieldsSchema), msg)
	wantInvalidArgument(t, e.updateRowKeySchema("t", other, true), msg)

	e.wantRowKeySchema(t, "t", &keySchema)
}

func TestUpdateTableRejectsRowKeySchemaClearWithoutIgnoreWarnings(t *testing.T) {
	e := newSchemaEnv(t)
	e.createTable(t, "t", &keySchema)

	wantInvalidArgument(t, e.updateRowKeySchema("t", nil, false),
		"Row key schema cannot be cleared without setting ignore_warnings to true.")
}

func TestUpdateTableClearsRowKeySchemaWithIgnoreWarnings(t *testing.T) {
	e := newSchemaEnv(t)
	e.createTable(t, "t", &keySchema)

	if err := e.admin.UpdateTableRemoveRowKeySchema(context.Background(), "t"); err != nil {
		t.Fatalf("UpdateTableRemoveRowKeySchema: %v", err)
	}
	e.wantRowKeySchema(t, "t", nil)
}

func (e *schemaEnv) write(t *testing.T, table, key string) {
	t.Helper()
	mut := bigtable.NewMutation()
	mut.Set("cf", "q", 1000, []byte("v"))
	if err := e.data.Open(table).Apply(context.Background(), key, mut); err != nil {
		t.Fatalf("Apply(%q): %v", key, err)
	}
}

func (e *schemaEnv) wantRow(t *testing.T, table, key string) {
	t.Helper()
	row, err := e.data.Open(table).ReadRow(context.Background(), key)
	if err != nil {
		t.Fatalf("ReadRow(%q): %v", key, err)
	}
	if row.Key() != key {
		t.Errorf("ReadRow(%q) returned row %q", key, row.Key())
	}
}

func TestUpdateTableSetsRowKeySchemaOverKeysThatDoNotFit(t *testing.T) {
	e := newSchemaEnv(t)
	e.createTable(t, "t", nil)
	e.write(t, "t", "no-delimiters")

	if err := e.admin.UpdateTableWithRowKeySchema(context.Background(), "t", keySchema); err != nil {
		t.Fatalf("UpdateTableWithRowKeySchema: %v", err)
	}
	e.wantRowKeySchema(t, "t", &keySchema)
}

func TestWritesIgnoreRowKeySchema(t *testing.T) {
	keys := map[string]string{
		"five fields":   "t1#p1#r1#i1#extra",
		"one field":     "t1",
		"invalid UTF-8": "t1#p1#\xff\xfe#i1",
	}
	e := newSchemaEnv(t)
	e.createTable(t, "t", &keySchema)
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			e.write(t, "t", key)
			e.wantRow(t, "t", key)
		})
	}
}

func TestUpdateRowKeySchemaKeepsDeletionProtection(t *testing.T) {
	ctx := context.Background()
	e := newSchemaEnv(t)
	e.createTable(t, "t", nil)
	if err := e.admin.UpdateTableWithDeletionProtection(ctx, "t", bigtable.Protected); err != nil {
		t.Fatalf("UpdateTableWithDeletionProtection: %v", err)
	}

	if err := e.admin.UpdateTableWithRowKeySchema(ctx, "t", keySchema); err != nil {
		t.Fatalf("UpdateTableWithRowKeySchema: %v", err)
	}
	if err := e.admin.UpdateTableRemoveRowKeySchema(ctx, "t"); err != nil {
		t.Fatalf("UpdateTableRemoveRowKeySchema: %v", err)
	}

	info, err := e.admin.TableInfo(ctx, "t")
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if info.DeletionProtection != bigtable.Protected {
		t.Errorf("deletion protection = %v, want Protected", info.DeletionProtection)
	}
}

const missingEncoding = "Missing encoding for STRUCT"

func TestCreateTableRejectsRowKeySchemaWithoutEncoding(t *testing.T) {
	ctx := context.Background()
	e := newSchemaEnv(t)

	conf := &bigtable.TableConf{TableID: "t", RowKeySchema: &bigtable.StructType{}}
	wantInvalidArgument(t, e.admin.CreateTableFromConf(ctx, conf), missingEncoding)

	if _, err := e.admin.TableInfo(ctx, "t"); status.Code(err) != codes.NotFound {
		t.Errorf("TableInfo error = %v, want NotFound", err)
	}
}

func TestUpdateTableRejectsRowKeySchemaWithoutEncoding(t *testing.T) {
	e := newSchemaEnv(t)
	e.createTable(t, "none", nil)
	e.createTable(t, "rks", &keySchema)

	for _, name := range []string{"none", "rks"} {
		wantInvalidArgument(t, e.admin.UpdateTableWithRowKeySchema(context.Background(), name, bigtable.StructType{}), missingEncoding)
		wantInvalidArgument(t, e.updateRowKeySchema(name, &btapb.Type_Struct{}, true), missingEncoding)
	}
	e.wantRowKeySchema(t, "none", nil)
	e.wantRowKeySchema(t, "rks", &keySchema)
}

func TestRowKeySchemaWithNoFieldsIsKept(t *testing.T) {
	e := newSchemaEnv(t)
	e.createTable(t, "created", &noFieldsSchema)
	e.createTable(t, "updated", nil)

	if err := e.admin.UpdateTableWithRowKeySchema(context.Background(), "updated", noFieldsSchema); err != nil {
		t.Fatalf("UpdateTableWithRowKeySchema: %v", err)
	}
	e.wantRowKeySchema(t, "created", &noFieldsSchema)
	e.wantRowKeySchema(t, "updated", &noFieldsSchema)
}

// Run under the race detector, this checks that GetTable reads the schema
// under the table's lock.
func TestGetTableDuringRowKeySchemaUpdates(t *testing.T) {
	ctx := context.Background()
	e := newSchemaEnv(t)
	e.createTable(t, "t", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			if _, err := e.admin.TableInfo(ctx, "t"); err != nil {
				t.Errorf("TableInfo: %v", err)
				return
			}
		}
	}()
	defer func() { <-done }()
	for range 20 {
		if err := e.admin.UpdateTableWithRowKeySchema(ctx, "t", keySchema); err != nil {
			t.Fatalf("UpdateTableWithRowKeySchema: %v", err)
		}
		if err := e.admin.UpdateTableRemoveRowKeySchema(ctx, "t"); err != nil {
			t.Fatalf("UpdateTableRemoveRowKeySchema: %v", err)
		}
	}
}
