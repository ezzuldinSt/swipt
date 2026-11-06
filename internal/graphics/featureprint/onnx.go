package featureprint

import (
	"encoding/binary"
	"math"

	E "github.com/sagernet/sing/common/exceptions"

	"google.golang.org/protobuf/encoding/protowire"
)

// The field numbers of onnx.proto (github.com/onnx/onnx, onnx/onnx.proto) this reader needs;
// every other field is skipped.
const (
	modelOpsetImport = 8
	modelGraph       = 7

	opsetDomain  = 1
	opsetVersion = 2

	graphNode        = 1
	graphInitializer = 5
	graphInput       = 11

	valueInfoName = 1

	nodeInput     = 1
	nodeOutput    = 2
	nodeOpType    = 4
	nodeAttribute = 5
	nodeDomain    = 7

	attributeName   = 1
	attributeFloat  = 2
	attributeInt    = 3
	attributeString = 4
	attributeTensor = 5
	attributeFloats = 7
	attributeInts   = 8

	tensorDims         = 1
	tensorDataType     = 2
	tensorFloatData    = 4
	tensorInt32Data    = 5
	tensorInt64Data    = 7
	tensorName         = 8
	tensorRawData      = 9
	tensorDataLocation = 14
)

// TensorProto.DataType values.
const (
	dataFloat = 1
	dataInt32 = 6
	dataInt64 = 7
)

var errMalformed = E.New("featureprint: the model is malformed")

type model struct {
	// opset is the version of the default operator set (domain "" or "ai.onnx").
	opset int64
	graph graph
}

type graph struct {
	nodes        []node
	initializers map[string]*tensor
	inputs       []string
}

type node struct {
	op         string
	domain     string
	inputs     []string
	outputs    []string
	attributes map[string]attribute
}

type attribute struct {
	f      float32
	i      int64
	s      []byte
	t      *tensor
	floats []float32
	ints   []int64
}

// tensor holds an initializer; only float and integer data are kept, integers widened.
type tensor struct {
	name     string
	dims     []int64
	dataType int64
	floats   []float32
	ints     []int64
}

func (t *tensor) elements() int {
	count := 1
	for _, dim := range t.dims {
		count *= int(dim)
	}
	return count
}

type field struct {
	number protowire.Number
	kind   protowire.Type
	value  []byte
}

// eachField walks the fields of an encoded message.
func eachField(data []byte, visit func(f field) error) error {
	for len(data) > 0 {
		number, kind, length := protowire.ConsumeTag(data)
		if length < 0 {
			return errMalformed
		}
		data = data[length:]
		size := protowire.ConsumeFieldValue(number, kind, data)
		if size < 0 {
			return errMalformed
		}
		err := visit(field{number: number, kind: kind, value: data[:size]})
		if err != nil {
			return err
		}
		data = data[size:]
	}
	return nil
}

func (f field) bytes() ([]byte, error) {
	if f.kind != protowire.BytesType {
		return nil, errMalformed
	}
	value, length := protowire.ConsumeBytes(f.value)
	if length < 0 {
		return nil, errMalformed
	}
	return value, nil
}

func (f field) text() (string, error) {
	value, err := f.bytes()
	return string(value), err
}

func (f field) varint() (int64, error) {
	if f.kind != protowire.VarintType {
		return 0, errMalformed
	}
	value, length := protowire.ConsumeVarint(f.value)
	if length < 0 {
		return 0, errMalformed
	}
	return int64(value), nil
}

func (f field) float() (float32, error) {
	if f.kind != protowire.Fixed32Type {
		return 0, errMalformed
	}
	value, length := protowire.ConsumeFixed32(f.value)
	if length < 0 {
		return 0, errMalformed
	}
	return math.Float32frombits(value), nil
}

// A parser accepts repeated scalars packed or not (protobuf encoding, "Packed Repeated Fields").
func (f field) appendFloats(to []float32) ([]float32, error) {
	if f.kind != protowire.BytesType {
		value, err := f.float()
		return append(to, value), err
	}
	packed, err := f.bytes()
	if err != nil || len(packed)%4 != 0 {
		return nil, errMalformed
	}
	for offset := 0; offset < len(packed); offset += 4 {
		to = append(to, math.Float32frombits(binary.LittleEndian.Uint32(packed[offset:])))
	}
	return to, nil
}

func (f field) appendInts(to []int64) ([]int64, error) {
	if f.kind != protowire.BytesType {
		value, err := f.varint()
		return append(to, value), err
	}
	packed, err := f.bytes()
	if err != nil {
		return nil, err
	}
	for len(packed) > 0 {
		value, length := protowire.ConsumeVarint(packed)
		if length < 0 {
			return nil, errMalformed
		}
		to = append(to, int64(value))
		packed = packed[length:]
	}
	return to, nil
}

// message decodes a nested message field.
func (f field) message(visit func(f field) error) error {
	value, err := f.bytes()
	if err != nil {
		return err
	}
	return eachField(value, visit)
}

func readModel(data []byte) (*model, error) {
	m := &model{opset: -1}
	err := eachField(data, func(f field) error {
		switch f.number {
		case modelOpsetImport:
			var domain string
			var version int64
			err := f.message(func(f field) (err error) {
				switch f.number {
				case opsetDomain:
					domain, err = f.text()
				case opsetVersion:
					version, err = f.varint()
				}
				return err
			})
			if err == nil && (domain == "" || domain == "ai.onnx") {
				m.opset = version
			}
			return err
		case modelGraph:
			value, err := f.bytes()
			if err != nil {
				return err
			}
			m.graph, err = readGraph(value)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if m.opset < 0 || m.graph.nodes == nil {
		return nil, errMalformed
	}
	return m, nil
}

func readGraph(data []byte) (graph, error) {
	g := graph{initializers: map[string]*tensor{}}
	err := eachField(data, func(f field) error {
		switch f.number {
		case graphNode:
			value, err := f.bytes()
			if err != nil {
				return err
			}
			n, err := readNode(value)
			if err == nil {
				g.nodes = append(g.nodes, n)
			}
			return err
		case graphInitializer:
			value, err := f.bytes()
			if err != nil {
				return err
			}
			t, err := readTensor(value)
			if err == nil {
				g.initializers[t.name] = t
			}
			return err
		case graphInput:
			return f.message(func(f field) error {
				if f.number != valueInfoName {
					return nil
				}
				name, err := f.text()
				g.inputs = append(g.inputs, name)
				return err
			})
		}
		return nil
	})
	return g, err
}

func readNode(data []byte) (node, error) {
	n := node{attributes: map[string]attribute{}}
	err := eachField(data, func(f field) error {
		var err error
		switch f.number {
		case nodeInput:
			var name string
			name, err = f.text()
			n.inputs = append(n.inputs, name)
		case nodeOutput:
			var name string
			name, err = f.text()
			n.outputs = append(n.outputs, name)
		case nodeOpType:
			n.op, err = f.text()
		case nodeDomain:
			n.domain, err = f.text()
		case nodeAttribute:
			var value []byte
			value, err = f.bytes()
			if err != nil {
				return err
			}
			var name string
			var a attribute
			name, a, err = readAttribute(value)
			n.attributes[name] = a
		}
		return err
	})
	return n, err
}

func readAttribute(data []byte) (string, attribute, error) {
	var name string
	var a attribute
	err := eachField(data, func(f field) error {
		var err error
		switch f.number {
		case attributeName:
			name, err = f.text()
		case attributeFloat:
			a.f, err = f.float()
		case attributeInt:
			a.i, err = f.varint()
		case attributeString:
			a.s, err = f.bytes()
		case attributeTensor:
			var value []byte
			value, err = f.bytes()
			if err != nil {
				return err
			}
			a.t, err = readTensor(value)
		case attributeFloats:
			a.floats, err = f.appendFloats(a.floats)
		case attributeInts:
			a.ints, err = f.appendInts(a.ints)
		}
		return err
	})
	return name, a, err
}

func readTensor(data []byte) (*tensor, error) {
	t := &tensor{}
	var raw []byte
	var external bool
	err := eachField(data, func(f field) error {
		var err error
		switch f.number {
		case tensorDims:
			t.dims, err = f.appendInts(t.dims)
		case tensorDataType:
			t.dataType, err = f.varint()
		case tensorFloatData:
			t.floats, err = f.appendFloats(t.floats)
		case tensorInt32Data, tensorInt64Data:
			t.ints, err = f.appendInts(t.ints)
		case tensorName:
			t.name, err = f.text()
		case tensorRawData:
			raw, err = f.bytes()
		case tensorDataLocation:
			var location int64
			location, err = f.varint()
			external = location != 0
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if external {
		return nil, E.New("featureprint: tensor ", t.name, " is stored outside the model")
	}
	// raw_data is little-endian whatever the host (onnx.proto, TensorProto.raw_data).
	if raw != nil {
		switch t.dataType {
		case dataFloat:
			if len(raw)%4 != 0 {
				return nil, errMalformed
			}
			t.floats = make([]float32, len(raw)/4)
			for index := range t.floats {
				t.floats[index] = math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4:]))
			}
		case dataInt64:
			if len(raw)%8 != 0 {
				return nil, errMalformed
			}
			t.ints = make([]int64, len(raw)/8)
			for index := range t.ints {
				t.ints[index] = int64(binary.LittleEndian.Uint64(raw[index*8:]))
			}
		case dataInt32:
			if len(raw)%4 != 0 {
				return nil, errMalformed
			}
			t.ints = make([]int64, len(raw)/4)
			for index := range t.ints {
				t.ints[index] = int64(int32(binary.LittleEndian.Uint32(raw[index*4:])))
			}
		}
	}
	switch t.dataType {
	case dataFloat:
		if len(t.floats) != t.elements() {
			return nil, errMalformed
		}
	case dataInt32, dataInt64:
		if len(t.ints) != t.elements() {
			return nil, errMalformed
		}
	}
	return t, nil
}
