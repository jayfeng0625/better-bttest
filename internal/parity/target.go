// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/jayfeng0625/better-bttest/bttest"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// A Target is a Bigtable that the cases run on: the real one, or an emulator.
type Target struct {
	Data  btpb.BigtableClient
	Admin adminpb.BigtableTableAdminClient
	// The admin service's long-running operations, which UpdateTable returns.
	Operations longrunningpb.OperationsClient
	Instance   string // projects/<project>/instances/<instance>
}

// The parity table's id on every target, so that status messages that name the table match across targets.
const parityTable = "better-bttest-parity"

func (t Target) tablePath(id string) string { return t.Instance + "/tables/" + id }

// How often a poll asks again, for an operation that is not done or an emulator that does not answer yet.
const pollInterval = 200 * time.Millisecond

func (t Target) wait(ctx context.Context, op *longrunningpb.Operation) error {
	for !op.Done {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		var err error
		if op, err = t.Operations.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name}); err != nil {
			return err
		}
	}
	return status.ErrorProto(op.GetError())
}

func (t Target) updateTable(ctx context.Context, tbl *adminpb.Table, field string, ignoreWarnings bool) error {
	op, err := t.Admin.UpdateTable(ctx, &adminpb.UpdateTableRequest{
		Table: tbl, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{field}}, IgnoreWarnings: ignoreWarnings,
	})
	if err != nil {
		return err
	}
	return t.wait(ctx, op)
}

// cmd/emulator serves with these message limits.
const maxMsgSize = 256 * 1024 * 1024

// StartGate starts the emulator from this checkout in-process, with the parity table, and returns it with its stop.
func StartGate(ctx context.Context) (Target, func(), error) {
	srv, err := bttest.NewServer("localhost:0", grpc.MaxRecvMsgSize(maxMsgSize), grpc.MaxSendMsgSize(maxMsgSize))
	if err != nil {
		return Target{}, nil, err
	}
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Close()
		return Target{}, nil, err
	}
	stop := func() {
		conn.Close()
		srv.Close()
	}
	t := emulatorTarget(conn)
	if err := createParityTable(ctx, t); err != nil {
		stop()
		return Target{}, nil, err
	}
	return t, stop, nil
}

func emulatorTarget(conn *grpc.ClientConn) Target {
	return Target{
		Data:       btpb.NewBigtableClient(conn),
		Admin:      adminpb.NewBigtableTableAdminClient(conn),
		Operations: longrunningpb.NewOperationsClient(conn),
		Instance:   "projects/parity/instances/parity",
	}
}

func createParityTable(ctx context.Context, t Target) error {
	_, err := t.Admin.CreateTable(ctx, &adminpb.CreateTableRequest{
		Parent: t.Instance, TableId: parityTable, Table: &adminpb.Table{ColumnFamilies: families()},
	})
	if err != nil {
		return fmt.Errorf("create the parity table: %w", err)
	}
	return nil
}

// DialReal connects to the real Bigtable with Application Default Credentials. The instance is
// projects/<project>/instances/<instance>.
func DialReal(ctx context.Context, instance string) (Target, func(), error) {
	data, err := gtransport.Dial(ctx, option.WithEndpoint("bigtable.googleapis.com:443"), option.WithScopes(bigtable.Scope))
	if err != nil {
		return Target{}, nil, err
	}
	admin, err := gtransport.Dial(ctx, option.WithEndpoint("bigtableadmin.googleapis.com:443"), option.WithScopes(bigtable.AdminScope))
	if err != nil {
		data.Close()
		return Target{}, nil, err
	}
	stop := func() {
		data.Close()
		admin.Close()
	}
	return Target{
		Data:       btpb.NewBigtableClient(data),
		Admin:      adminpb.NewBigtableTableAdminClient(admin),
		Operations: longrunningpb.NewOperationsClient(admin),
		Instance:   instance,
	}, stop, nil
}

// An emulator in a Docker container, which a case can stop.
type Container struct {
	Target
	ID string
}

// StartContainer runs the image with the command as an emulator that listens on port 8086, waits for it to answer,
// and creates the parity table in it. The label marks the container for removal.
func StartContainer(ctx context.Context, image string, command []string, label string) (Container, func(), error) {
	args := append([]string{"run", "-d", "-p", "127.0.0.1::8086", "--label", label, image}, command...)
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return Container{}, nil, fmt.Errorf("docker run %s: %w", image, err)
	}
	id := strings.TrimSpace(string(out))
	remove := func() { exec.Command("docker", "rm", "-f", id).Run() }
	port, err := exec.CommandContext(ctx, "docker", "port", id, "8086/tcp").Output()
	if err != nil {
		remove()
		return Container{}, nil, fmt.Errorf("docker port: %w", err)
	}
	conn, err := grpc.NewClient(strings.Fields(string(port))[0], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		remove()
		return Container{}, nil, err
	}
	stop := func() {
		conn.Close()
		remove()
	}
	c := Container{ID: id, Target: emulatorTarget(conn)}
	for {
		_, err := c.Admin.ListTables(ctx, &adminpb.ListTablesRequest{Parent: c.Instance})
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			stop()
			return Container{}, nil, fmt.Errorf("the emulator in %s did not answer: %w", image, err)
		case <-time.After(pollInterval):
		}
	}
	if err := createParityTable(ctx, c.Target); err != nil {
		stop()
		return Container{}, nil, err
	}
	return c, stop, nil
}

// Stopped returns the panic in the container's log, or its last lines, once its emulator no longer answers. The
// container can outlive its emulator, since gcloud runs the emulator as a child process.
func (c Container) Stopped() (log string, stopped bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Admin.ListTables(ctx, &adminpb.ListTablesRequest{Parent: c.Instance}); err == nil {
		return "", false
	}
	out, _ := exec.Command("docker", "logs", c.ID).CombinedOutput()
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if strings.Contains(line, "panic") {
			return strings.Join(lines[i:min(i+7, len(lines))], "\n"), true
		}
	}
	return strings.Join(lines[max(0, len(lines)-20):], "\n"), true
}
