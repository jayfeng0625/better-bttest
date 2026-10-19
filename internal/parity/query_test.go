// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"io"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

type scriptedStream []*btpb.ExecuteQueryResponse

func (s *scriptedStream) Recv() (*btpb.ExecuteQueryResponse, error) {
	if len(*s) == 0 {
		return nil, io.EOF
	}
	resp := (*s)[0]
	*s = (*s)[1:]
	return resp, nil
}

type part struct {
	reset, resumable bool
	values           []int64
}

func message(t *testing.T, p part) *btpb.ExecuteQueryResponse {
	t.Helper()
	results := &btpb.PartialResultSet{Reset_: p.reset}
	if p.values != nil {
		var rows btpb.ProtoRows
		for _, v := range p.values {
			rows.Values = append(rows.Values, intValue(v))
		}
		data, err := proto.Marshal(&rows)
		if err != nil {
			t.Fatal(err)
		}
		results.PartialRows = &btpb.PartialResultSet_ProtoRowsBatch{ProtoRowsBatch: &btpb.ProtoRowsBatch{BatchData: data}}
	}
	if p.resumable {
		checksum := uint32(1)
		results.BatchChecksum = &checksum
		results.ResumeToken = []byte("token")
	}
	return &btpb.ExecuteQueryResponse{Response: &btpb.ExecuteQueryResponse_Results{Results: results}}
}

func intValue(v int64) *btpb.Value {
	return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: v}}
}

func decodeParts(t *testing.T, width int, parts ...part) Result {
	t.Helper()
	stream := make(scriptedStream, len(parts))
	for i, p := range parts {
		stream[i] = message(t, p)
	}
	res, err := decodeQuery(&stream, width)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func checkDecoded(t *testing.T, got Result, wantRows [][]*btpb.Value, wantMessages []string) {
	t.Helper()
	if d := cmp.Diff(wantRows, got.Rows, protocmp.Transform()); d != "" {
		t.Errorf("rows (-want +got):\n%s", d)
	}
	if d := cmp.Diff(wantMessages, got.Messages); d != "" {
		t.Errorf("messages (-want +got):\n%s", d)
	}
}

func TestDecodeQueryDropsAPartialBatchOnReset(t *testing.T) {
	got := decodeParts(t, 2,
		part{values: []int64{1}},
		part{reset: true, resumable: true, values: []int64{1, 2}},
	)
	checkDecoded(t, got,
		[][]*btpb.Value{{intValue(1), intValue(2)}},
		[]string{"batch rows=0", "batch rows=1 reset checksum token"},
	)
}

func TestDecodeQueryDropsTheRowsSinceTheLastTokenOnReset(t *testing.T) {
	got := decodeParts(t, 1,
		part{resumable: true, values: []int64{1}},
		part{values: []int64{2, 3}},
		part{reset: true, resumable: true, values: []int64{2, 3}},
	)
	checkDecoded(t, got,
		[][]*btpb.Value{{intValue(1)}, {intValue(2)}, {intValue(3)}},
		[]string{"batch rows=1 checksum token", "batch rows=2", "batch rows=2 reset checksum token"},
	)
}

func TestDecodeQueryKeepsTheRowsOfAResetOnTheFirstMessage(t *testing.T) {
	got := decodeParts(t, 1,
		part{reset: true, resumable: true, values: []int64{1, 2}},
		part{resumable: true, values: []int64{3}},
	)
	checkDecoded(t, got,
		[][]*btpb.Value{{intValue(1)}, {intValue(2)}, {intValue(3)}},
		[]string{"batch rows=2 reset checksum token", "batch rows=1 checksum token"},
	)
}
