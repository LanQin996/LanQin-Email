package nativedb

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

func headerFixture() Header {
	return Header{Version: 1, Driver: "mysql", Family: "mysql:8.4", SchemaVersion: 12, Tables: []Table{{Name: "messages", Columns: []Column{{Name: "body", Type: "text"}, {Name: "id", Type: "bigint"}, {Name: "optional", Type: "text", Nullable: true}}}}}
}

func validFixture(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := NewWriter(&output, headerFixture(), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Row("messages", []Cell{{Kind: "text", Data: "合成邮件\n'); DROP TABLE users;--"}, {Kind: "int", Data: "9223372036854775807"}, {Kind: "null"}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestTypedSnapshotRoundTripPreservesValuesWithoutSQL(t *testing.T) {
	input := validFixture(t)
	rows := 0
	header, count, err := Read(t.Context(), bytes.NewReader(input), DefaultLimits(), nil, func(table string, values []Cell) error {
		rows++
		if table != "messages" || values[0].Data != "合成邮件\n'); DROP TABLE users;--" {
			t.Fatal("text was interpreted or changed")
		}
		value, err := values[1].Value(1024)
		if err != nil || value != int64(math.MaxInt64) {
			t.Fatal("64-bit integer lost precision")
		}
		return nil
	})
	if err != nil || header.Driver != "mysql" || count != 1 || rows != 1 {
		t.Fatal("native format roundtrip", err)
	}
	for _, value := range []any{nil, []byte{0, 255, 1}, "", int64(math.MinInt64), true, false, 0.25, time.Date(2026, 10, 31, 1, 2, 3, 123, time.UTC)} {
		cell, err := EncodeCell(value, false, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cell.Value(1024); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotRejectsIncompleteTamperedOrUnboundedRecords(t *testing.T) {
	input := validFixture(t)
	for name, data := range map[string][]byte{
		"truncated":      input[:len(input)-1],
		"missing-footer": input[:bytes.LastIndex(input, []byte(`{"kind":"complete"`))],
		"trailing":       append(append([]byte{}, input...), []byte("{}\n")...),
		"changed-row":    bytes.Replace(input, []byte("9223372036854775807"), []byte("9223372036854775806"), 1),
		"unknown-field":  bytes.Replace(input, []byte(`"kind":"row"`), []byte(`"kind":"row","sql":"DROP TABLE users"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Read(t.Context(), bytes.NewReader(data), DefaultLimits(), nil, nil); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
	for _, limits := range []Limits{{MaxBytes: 10, MaxRows: 1, MaxRecordBytes: 100, MaxCellBytes: 10}, {MaxBytes: 1 << 20, MaxRows: 1, MaxRecordBytes: 16, MaxCellBytes: 10}} {
		if _, _, err := Read(t.Context(), bytes.NewReader(input), limits, nil, nil); err == nil {
			t.Fatal("resource limit bypassed")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := Read(ctx, bytes.NewReader(input), DefaultLimits(), nil, nil); err == nil {
		t.Fatal("cancelled validation continued")
	}
}

func TestFormatRejectsUnsafeSchemaAndCells(t *testing.T) {
	for _, name := range []string{"../messages", "messages;DROP TABLE users", "UserTable", strings.Repeat("a", 65)} {
		header := headerFixture()
		header.Tables[0].Name = name
		if header.Validate() == nil {
			t.Fatal("unsafe identifier accepted")
		}
	}
	for _, cell := range []Cell{{Kind: "int", Data: "9223372036854775808"}, {Kind: "float", Data: "NaN"}, {Kind: "binary", Data: "not-base64"}, {Kind: "bool", Data: "yes"}, {Kind: "null", Data: "payload"}, {Kind: "sql", Data: "SELECT 1"}} {
		if _, err := cell.Value(1024); err == nil {
			t.Fatal("invalid cell accepted")
		}
	}
	if _, err := EncodeCell(string([]byte{0xff}), true, 1024); err == nil {
		t.Fatal("invalid UTF-8 was silently normalized")
	}
}
