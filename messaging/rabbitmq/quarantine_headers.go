package rabbitmq

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"sort"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Header values are tagged rather than decoded through JSON numbers. Bytes,
// strings (including invalid UTF-8), integer widths and floating point bits
// survive audit storage without coercion.
type quarantineHeader struct {
	Type   string                  `json:"type"`
	Data   []byte                  `json:"data,omitempty"`
	Fields []quarantineHeaderField `json:"fields,omitempty"`
	Items  []quarantineHeader      `json:"items,omitempty"`
}

type quarantineHeaderField struct {
	Key   []byte           `json:"key"`
	Value quarantineHeader `json:"value"`
}

var quarantineScalarTypes = map[string]reflect.Type{
	"bool": reflect.TypeOf(false), "uint8": reflect.TypeOf(uint8(0)),
	"int8": reflect.TypeOf(int8(0)), "int16": reflect.TypeOf(int16(0)),
	"int32": reflect.TypeOf(int32(0)), "int64": reflect.TypeOf(int64(0)),
	"uint16": reflect.TypeOf(uint16(0)), "uint32": reflect.TypeOf(uint32(0)),
	"float32": reflect.TypeOf(float32(0)), "float64": reflect.TypeOf(float64(0)),
	"decimal": reflect.TypeOf(amqp.Decimal{}),
}

type quarantineBudget struct{ nodes, bytes int }

func (b *quarantineBudget) take(depth, size int) error {
	b.nodes++
	b.bytes += size
	if depth > 16 || b.nodes > 4096 || b.bytes > MaxQuarantineBytes {
		return ErrQuarantineEnvelope
	}
	return nil
}

func encodeQuarantineHeader(value any, depth int, budget *quarantineBudget) (quarantineHeader, error) {
	if err := budget.take(depth, 0); err != nil {
		return quarantineHeader{}, err
	}
	n := quarantineHeader{}
	switch v := value.(type) {
	case nil:
		n.Type = "null"
	case amqp.Table:
		n.Type = "table"
		if v == nil {
			n.Type = "table_nil"
			break
		}
		if len(v) > 4096 {
			return n, ErrQuarantineEnvelope
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := budget.take(depth, len(key)); err != nil {
				return n, err
			}
			child, err := encodeQuarantineHeader(v[key], depth+1, budget)
			if err != nil {
				return n, err
			}
			n.Fields = append(n.Fields, quarantineHeaderField{[]byte(key), child})
		}
	case []any:
		n.Type = "array"
		if v == nil {
			n.Type = "array_nil"
			break
		}
		if len(v) > 4096 {
			return n, ErrQuarantineEnvelope
		}
		for _, item := range v {
			child, err := encodeQuarantineHeader(item, depth+1, budget)
			if err != nil {
				return n, err
			}
			n.Items = append(n.Items, child)
		}
	case []byte:
		n.Type = "bytes"
		if v == nil {
			n.Type = "bytes_nil"
		}
		if err := budget.take(depth, len(v)); err != nil {
			return n, err
		}
		n.Data = append([]byte(nil), v...)
	case string:
		n.Type = "string"
		if err := budget.take(depth, len(v)); err != nil {
			return n, err
		}
		n.Data = []byte(v)
	case time.Time:
		n.Type = "time"
		var err error
		n.Data, err = v.UTC().MarshalBinary()
		if err != nil {
			return n, ErrQuarantineEnvelope
		}
	default:
		n.Type = reflect.TypeOf(value).Kind().String()
		if _, ok := value.(amqp.Decimal); ok {
			n.Type = "decimal"
		}
		if n.Type == "int" {
			if reflect.TypeOf(value) != reflect.TypeOf(int(0)) {
				return n, ErrQuarantineEnvelope
			}
			value = int64(value.(int))
		} else if typ, ok := quarantineScalarTypes[n.Type]; !ok || reflect.TypeOf(value) != typ {
			return n, ErrQuarantineEnvelope
		}
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.BigEndian, value); err != nil {
			return n, ErrQuarantineEnvelope
		}
		n.Data = buf.Bytes()
	}
	return n, nil
}

func decodeQuarantineHeader(n quarantineHeader, depth int, budget *quarantineBudget) (any, error) {
	if err := budget.take(depth, len(n.Data)); err != nil {
		return nil, err
	}
	if n.Type != "table" && len(n.Fields) != 0 || n.Type != "array" && len(n.Items) != 0 {
		return nil, ErrQuarantineEnvelope
	}
	switch n.Type {
	case "null", "table_nil", "array_nil", "bytes_nil":
		if len(n.Data) != 0 {
			return nil, ErrQuarantineEnvelope
		}
		switch n.Type {
		case "table_nil":
			return amqp.Table(nil), nil
		case "array_nil":
			return []any(nil), nil
		case "bytes_nil":
			return []byte(nil), nil
		}
		return nil, nil
	case "table":
		if len(n.Data) != 0 {
			return nil, ErrQuarantineEnvelope
		}
		out := make(amqp.Table, len(n.Fields))
		for i, field := range n.Fields {
			if err := budget.take(depth, len(field.Key)); err != nil {
				return nil, err
			}
			if i > 0 && bytes.Compare(n.Fields[i-1].Key, field.Key) >= 0 {
				return nil, ErrQuarantineEnvelope
			}
			value, err := decodeQuarantineHeader(field.Value, depth+1, budget)
			if err != nil {
				return nil, err
			}
			out[string(field.Key)] = value
		}
		return out, nil
	case "array":
		if len(n.Data) != 0 {
			return nil, ErrQuarantineEnvelope
		}
		out := make([]any, 0, len(n.Items))
		for _, item := range n.Items {
			value, err := decodeQuarantineHeader(item, depth+1, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case "bytes":
		return append([]byte{}, n.Data...), nil
	case "string":
		return string(n.Data), nil
	case "time":
		var stamp time.Time
		if err := stamp.UnmarshalBinary(n.Data); err != nil {
			return nil, ErrQuarantineEnvelope
		}
		return stamp.UTC(), nil
	default:
		typ, ok := quarantineScalarTypes[n.Type]
		if n.Type == "int" {
			typ, ok = reflect.TypeOf(int64(0)), true
		}
		if !ok {
			return nil, ErrQuarantineEnvelope
		}
		value := reflect.New(typ)
		if len(n.Data) != binary.Size(value.Elem().Interface()) {
			return nil, ErrQuarantineEnvelope
		}
		if err := binary.Read(bytes.NewReader(n.Data), binary.BigEndian, value.Interface()); err != nil {
			return nil, ErrQuarantineEnvelope
		}
		if n.Type == "int" {
			x := value.Elem().Int()
			if int64(int(x)) != x {
				return nil, ErrQuarantineEnvelope
			}
			return int(x), nil
		}
		return value.Elem().Interface(), nil
	}
}
