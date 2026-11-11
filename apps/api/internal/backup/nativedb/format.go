// Package nativedb defines a bounded, typed logical database snapshot. It never
// executes SQL from an archive. The application exporter/importer must bind this
// format to a consistent source transaction and its own known target schema.
package nativedb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const Filename = "data/lanqin-native.jsonl"

var ErrInvalid = errors.New("native database snapshot is invalid, incomplete, unsupported or exceeds limits")
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var family = regexp.MustCompile(`^(mysql|postgres):[0-9]+(?:\.[0-9]+)?$`)

type Limits struct {
	MaxBytes       int64
	MaxRows        int64
	MaxRecordBytes int
	MaxCellBytes   int
}

func DefaultLimits() Limits {
	return Limits{MaxBytes: 10 << 30, MaxRows: 1000000, MaxRecordBytes: 128 << 20, MaxCellBytes: 64 << 20}
}
func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxBytes <= 1<<40 && l.MaxRows > 0 && l.MaxRows <= 100000000 && l.MaxRecordBytes > 0 && l.MaxRecordBytes <= 128<<20 && l.MaxCellBytes > 0 && l.MaxCellBytes <= 64<<20
}

type Column struct {
	Name              string `json:"name"`
	Type              string `json:"type"`
	Nullable          bool   `json:"nullable"`
	Generated         bool   `json:"generated,omitempty"`
	MaxLength         int64  `json:"maxLength,omitempty"`
	Precision         int64  `json:"precision,omitempty"`
	Scale             int64  `json:"scale,omitempty"`
	DatetimePrecision int64  `json:"datetimePrecision,omitempty"`
	Collation         string `json:"collation,omitempty"`
}

type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

type Header struct {
	Version       int     `json:"version"`
	Driver        string  `json:"driver"`
	Family        string  `json:"family"`
	SchemaVersion int     `json:"schemaVersion"`
	Tables        []Table `json:"tables"`
}

// Cell encodes integer/numeric values as strings, preserving 64-bit precision.
// Text is validated UTF-8; binary values use strict base64, never SQL literals.
type Cell struct {
	Kind string `json:"kind"`
	Data string `json:"data,omitempty"`
}

type Record struct {
	Kind   string  `json:"kind"`
	Header *Header `json:"header,omitempty"`
	Table  string  `json:"table,omitempty"`
	Values []Cell  `json:"values,omitempty"`
	Rows   int64   `json:"rows,omitempty"`
	SHA256 string  `json:"sha256,omitempty"`
}

func (h Header) Validate() error {
	if h.Version != 1 || h.SchemaVersion < 1 || h.SchemaVersion > 100000 || h.Driver != "mysql" && h.Driver != "postgres" || !family.MatchString(h.Family) || !strings.HasPrefix(h.Family, h.Driver+":") || len(h.Tables) < 1 || len(h.Tables) > 128 {
		return ErrInvalid
	}
	previous := ""
	for _, table := range h.Tables {
		if !identifier.MatchString(table.Name) || table.Name <= previous || len(table.Columns) < 1 || len(table.Columns) > 256 {
			return ErrInvalid
		}
		previous = table.Name
		last := ""
		base := 0
		for _, column := range table.Columns {
			if !identifier.MatchString(column.Name) || column.Name <= last || column.Type == "" || len(column.Type) > 128 || len(column.Collation) > 128 || !utf8.ValidString(column.Type+column.Collation) || strings.ContainsAny(column.Type+column.Collation, "\x00\r\n") || column.MaxLength < 0 || column.Precision < 0 || column.Scale < 0 || column.DatetimePrecision < 0 || column.DatetimePrecision > 9 {
				return ErrInvalid
			}
			last = column.Name
			if !column.Generated {
				base++
			}
		}
		if base == 0 {
			return ErrInvalid
		}
	}
	return nil
}

func EncodeCell(value any, textual bool, limit int) (Cell, error) {
	cell := Cell{}
	switch value := value.(type) {
	case nil:
		cell.Kind = "null"
	case string:
		if !utf8.ValidString(value) || len(value) > limit {
			return cell, ErrInvalid
		}
		cell.Kind, cell.Data = "utf8", base64.StdEncoding.EncodeToString([]byte(value))
	case []byte:
		if len(value) > limit {
			return cell, ErrInvalid
		}
		if textual {
			if !utf8.Valid(value) {
				return cell, ErrInvalid
			}
			cell.Kind, cell.Data = "utf8", base64.StdEncoding.EncodeToString(value)
		} else {
			cell.Kind, cell.Data = "binary", base64.StdEncoding.EncodeToString(value)
		}
	case int64:
		cell.Kind, cell.Data = "int", strconv.FormatInt(value, 10)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return cell, ErrInvalid
		}
		cell.Kind, cell.Data = "float", strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		cell.Kind = "bool"
		if value {
			cell.Data = "1"
		} else {
			cell.Data = "0"
		}
	case time.Time:
		if value.IsZero() {
			return cell, ErrInvalid
		}
		cell.Kind, cell.Data = "time", value.Format(time.RFC3339Nano)
	default:
		return cell, ErrInvalid
	}
	return cell, nil
}

func (c Cell) Value(limit int) (any, error) {
	if len(c.Data) > limit*2 || !utf8.ValidString(c.Data) {
		return nil, ErrInvalid
	}
	switch c.Kind {
	case "null":
		if c.Data != "" {
			return nil, ErrInvalid
		}
		return nil, nil
	case "text":
		if len(c.Data) > limit {
			return nil, ErrInvalid
		}
		return c.Data, nil
	case "binary":
		data, err := base64.StdEncoding.Strict().DecodeString(c.Data)
		if err != nil || len(data) > limit {
			return nil, ErrInvalid
		}
		return data, nil
	case "utf8":
		data, err := base64.StdEncoding.Strict().DecodeString(c.Data)
		if err != nil || len(data) > limit || !utf8.Valid(data) {
			return nil, ErrInvalid
		}
		return string(data), nil
	case "int":
		n, err := strconv.ParseInt(c.Data, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != c.Data {
			return nil, ErrInvalid
		}
		return n, nil
	case "float":
		n, err := strconv.ParseFloat(c.Data, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, ErrInvalid
		}
		return n, nil
	case "bool":
		if c.Data == "1" {
			return true, nil
		}
		if c.Data == "0" {
			return false, nil
		}
		return nil, ErrInvalid
	case "time":
		value, err := time.Parse(time.RFC3339Nano, c.Data)
		if err != nil || value.IsZero() {
			return nil, ErrInvalid
		}
		return value, nil
	default:
		return nil, ErrInvalid
	}
}

type Writer struct {
	dst         io.Writer
	digest      hash.Hash
	header      Header
	tables      map[string][]Column
	limits      Limits
	bytes, rows int64
	closed      bool
}

func NewWriter(dst io.Writer, header Header, limits Limits) (*Writer, error) {
	if dst == nil || !limits.valid() || header.Validate() != nil {
		return nil, ErrInvalid
	}
	writer := &Writer{dst: dst, digest: sha256.New(), header: header, tables: baseColumns(header), limits: limits}
	if err := writer.write(Record{Kind: "header", Header: &header}, true); err != nil {
		return nil, err
	}
	return writer, nil
}

func baseColumns(header Header) map[string][]Column {
	tables := map[string][]Column{}
	for _, table := range header.Tables {
		for _, column := range table.Columns {
			if !column.Generated {
				tables[table.Name] = append(tables[table.Name], column)
			}
		}
	}
	return tables
}

func validateRow(columns []Column, values []Cell, limit int) error {
	if len(columns) == 0 || len(values) != len(columns) {
		return ErrInvalid
	}
	for i, cell := range values {
		value, err := cell.Value(limit)
		if err != nil || value == nil && !columns[i].Nullable {
			return ErrInvalid
		}
	}
	return nil
}

func (w *Writer) Row(table string, values []Cell) error {
	if w.closed || w.rows >= w.limits.MaxRows || validateRow(w.tables[table], values, w.limits.MaxCellBytes) != nil {
		return ErrInvalid
	}
	if err := w.write(Record{Kind: "row", Table: table, Values: values}, true); err != nil {
		return err
	}
	w.rows++
	return nil
}

func (w *Writer) Close() error {
	if w.closed {
		return ErrInvalid
	}
	w.closed = true
	return w.write(Record{Kind: "complete", Rows: w.rows, SHA256: hex.EncodeToString(w.digest.Sum(nil))}, false)
}

func (w *Writer) write(record Record, hashed bool) error {
	data, err := json.Marshal(record)
	if err != nil || len(data)+1 > w.limits.MaxRecordBytes || int64(len(data)+1) > w.limits.MaxBytes-w.bytes {
		return ErrInvalid
	}
	data = append(data, '\n')
	if n, err := w.dst.Write(data); err != nil || n != len(data) {
		return ErrInvalid
	}
	w.bytes += int64(len(data))
	if hashed {
		w.digest.Write(data)
	}
	return nil
}

// Read validates every record, footer hash and authenticated-format EOF before
// returning. Callers importing rows must use a rollbackable target transaction;
// format validation is not proof of sender identity or archive authentication.
func Read(ctx context.Context, source io.Reader, limits Limits, onHeader func(Header) error, onRow func(string, []Cell) error) (Header, int64, error) {
	var header Header
	if source == nil || !limits.valid() {
		return header, 0, ErrInvalid
	}
	reader := bufio.NewReaderSize(io.LimitReader(source, limits.MaxBytes+1), 64<<10)
	digest := sha256.New()
	var tables map[string][]Column
	var total, rows int64
	for index := int64(0); ; index++ {
		if ctx.Err() != nil {
			return header, rows, ctx.Err()
		}
		line, err := readLine(reader, limits.MaxRecordBytes)
		if err != nil {
			return header, rows, ErrInvalid
		}
		total += int64(len(line))
		if total > limits.MaxBytes {
			return header, rows, ErrInvalid
		}
		var record Record
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF {
			return header, rows, ErrInvalid
		}
		if index == 0 {
			if record.Kind != "header" || record.Header == nil || record.Header.Validate() != nil || record.Table != "" || len(record.Values) > 0 || record.Rows != 0 || record.SHA256 != "" {
				return header, rows, ErrInvalid
			}
			header = *record.Header
			tables = baseColumns(header)
			if onHeader != nil {
				if err := onHeader(header); err != nil {
					return header, rows, err
				}
			}
		} else if record.Kind == "row" {
			if record.Header != nil || record.SHA256 != "" || record.Rows != 0 || rows >= limits.MaxRows || validateRow(tables[record.Table], record.Values, limits.MaxCellBytes) != nil {
				return header, rows, ErrInvalid
			}
			if onRow != nil {
				if err := onRow(record.Table, record.Values); err != nil {
					return header, rows, err
				}
			}
			rows++
		} else if record.Kind == "complete" {
			if record.Header != nil || record.Table != "" || len(record.Values) > 0 || record.Rows != rows || record.SHA256 != hex.EncodeToString(digest.Sum(nil)) {
				return header, rows, ErrInvalid
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				return header, rows, ErrInvalid
			}
			return header, rows, nil
		} else {
			return header, rows, ErrInvalid
		}
		digest.Write(line)
	}
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	line := []byte{}
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > limit-len(line) {
			return nil, ErrInvalid
		}
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, ErrInvalid
		}
	}
}
