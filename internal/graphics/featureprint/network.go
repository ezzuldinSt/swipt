package featureprint

import (
	"context"
	"math"
	"slices"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
)

// network runs the part of an ONNX graph that makes the output of its GlobalAveragePool, for
// one image (batch 1). Tensors are float32 in NCHW order without the batch dimension.
type network struct {
	steps []step
	// sizes is the floats of each tensor and slots the workspace buffer it lives in, by id.
	sizes    []int
	slots    []int
	capacity []int
	scratch  int
	input    int
	output   int
	pool     sync.Pool
}

type step interface {
	// uses answers the tensors the step reads and the one it writes.
	uses() (inputs []int, output int)
	run(w *workspace)
}

type workspace struct {
	network *network
	buffers [][]float32
	scratch []float32
}

func (w *workspace) tensor(id int) []float32 {
	return w.buffers[w.network.slots[id]][:w.network.sizes[id]]
}

type shape struct {
	channels, height, width int
}

func (s shape) area() int {
	return s.height * s.width
}

func (s shape) size() int {
	return s.channels * s.height * s.width
}

// activation clamps values to [low, high]: Relu is [0, +Inf), Clip its bounds.
type activation struct {
	low, high float32
}

var identity = activation{low: float32(math.Inf(-1)), high: float32(math.Inf(1))}

func (a activation) apply(values []float32) {
	if a == identity {
		return
	}
	for index, value := range values {
		values[index] = min(max(value, a.low), a.high)
	}
}

type compiler struct {
	graph     *graph
	opset     int64
	ids       map[string]int
	shapes    map[string]shape
	consumers map[string][]int
	fused     []bool
	steps     []step
	sizes     []int
	scratch   int
}

// compile plans the nodes the pooled features need, for an RGB input of side by side pixels.
func compile(m *model, side int) (*network, error) {
	g := &m.graph
	target := ""
	for _, n := range g.nodes {
		if n.op != "GlobalAveragePool" {
			continue
		}
		if target != "" || len(n.outputs) != 1 {
			return nil, E.New("featureprint: the model needs one GlobalAveragePool")
		}
		target = n.outputs[0]
	}
	if target == "" {
		return nil, E.New("featureprint: the model has no GlobalAveragePool")
	}
	input := ""
	for _, name := range g.inputs {
		if g.initializers[name] == nil {
			input = name
			break
		}
	}
	if input == "" {
		return nil, E.New("featureprint: the model has no input")
	}
	producers := map[string]int{}
	for index, n := range g.nodes {
		for _, output := range n.outputs {
			producers[output] = index
		}
	}
	needed := make([]bool, len(g.nodes))
	pending := []string{target}
	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if name == "" || name == input || g.initializers[name] != nil {
			continue
		}
		index, found := producers[name]
		if !found {
			return nil, E.New("featureprint: nothing in the model makes ", name)
		}
		if !needed[index] {
			needed[index] = true
			pending = append(pending, g.nodes[index].inputs...)
		}
	}
	c := &compiler{
		graph:     g,
		opset:     m.opset,
		ids:       map[string]int{},
		shapes:    map[string]shape{},
		consumers: map[string][]int{},
		fused:     make([]bool, len(g.nodes)),
	}
	for index, n := range g.nodes {
		if needed[index] {
			for _, name := range n.inputs {
				c.consumers[name] = append(c.consumers[name], index)
			}
		}
	}
	first, err := c.define(input, shape{channels: 3, height: side, width: side})
	if err != nil {
		return nil, err
	}
	// The ONNX IR requires nodes in topological order (IR.md, "Graphs").
	for index, n := range g.nodes {
		if !needed[index] || c.fused[index] {
			continue
		}
		if n.domain != "" && n.domain != "ai.onnx" {
			return nil, E.New("featureprint: operator ", n.domain, ".", n.op, " is not supported")
		}
		switch n.op {
		case "Conv":
			err = c.conv(n)
		case "Relu", "Clip":
			err = c.activation(n)
		case "Add":
			err = c.add(n)
		case "GlobalAveragePool":
			err = c.globalAveragePool(n)
		default:
			err = E.New("featureprint: operator ", n.op, " is not supported")
		}
		if err != nil {
			return nil, err
		}
	}
	output, found := c.ids[target]
	if !found {
		return nil, E.New("featureprint: the model never makes ", target)
	}
	net := &network{steps: c.steps, sizes: c.sizes, scratch: c.scratch, input: first, output: output}
	net.allocate()
	net.pool.New = func() any {
		w := &workspace{network: net, buffers: make([][]float32, len(net.capacity)), scratch: make([]float32, net.scratch)}
		for slot, capacity := range net.capacity {
			w.buffers[slot] = make([]float32, capacity)
		}
		return w
	}
	return net, nil
}

func (c *compiler) define(name string, s shape) (int, error) {
	if _, exists := c.ids[name]; exists || name == "" {
		return 0, E.New("featureprint: tensor ", name, " is made twice")
	}
	if s.channels < 1 || s.height < 1 || s.width < 1 {
		return 0, E.New("featureprint: tensor ", name, " is empty")
	}
	id := len(c.sizes)
	c.ids[name] = id
	c.shapes[name] = s
	c.sizes = append(c.sizes, s.size())
	return id, nil
}

func (c *compiler) tensor(name string) (int, shape, error) {
	id, found := c.ids[name]
	if !found {
		return 0, shape{}, E.New("featureprint: tensor ", name, " is not computed")
	}
	return id, c.shapes[name], nil
}

func (c *compiler) weight(name string) (*tensor, error) {
	t := c.graph.initializers[name]
	if t == nil || t.dataType != dataFloat {
		return nil, E.New("featureprint: ", name, " is not a float initializer")
	}
	return t, nil
}

func (c *compiler) ints(n node, name string, fallback ...int64) []int64 {
	if a, found := n.attributes[name]; found {
		return a.ints
	}
	return fallback
}

// absorb finds the Relu or Clip a node's only consumer applies, which the node then applies
// itself; it answers the activation and the name of the tensor it makes.
func (c *compiler) absorb(output string) (activation, string, error) {
	consumers := c.consumers[output]
	if len(consumers) != 1 {
		return identity, output, nil
	}
	n := c.graph.nodes[consumers[0]]
	if (n.op != "Relu" && n.op != "Clip") || len(n.outputs) != 1 || len(n.inputs) == 0 || n.inputs[0] != output {
		return identity, output, nil
	}
	a, err := c.bounds(n)
	if err != nil {
		return identity, "", err
	}
	c.fused[consumers[0]] = true
	return a, n.outputs[0], nil
}

// bounds reads the range of Relu or Clip; Clip takes it from attributes before opset 11 and
// from optional inputs since (Operators.md, Clip).
func (c *compiler) bounds(n node) (activation, error) {
	if n.op == "Relu" {
		return activation{low: 0, high: identity.high}, nil
	}
	a := identity
	if c.opset < 11 {
		if value, found := n.attributes["min"]; found {
			a.low = value.f
		}
		if value, found := n.attributes["max"]; found {
			a.high = value.f
		}
		return a, nil
	}
	for index, bound := range []*float32{&a.low, &a.high} {
		if index+1 >= len(n.inputs) || n.inputs[index+1] == "" {
			continue
		}
		t, err := c.weight(n.inputs[index+1])
		if err != nil {
			return identity, err
		}
		if len(t.floats) != 1 {
			return identity, E.New("featureprint: the bound ", t.name, " is not a scalar")
		}
		*bound = t.floats[0]
	}
	return a, nil
}

func (c *compiler) conv(n node) error {
	if len(n.inputs) < 2 || len(n.outputs) != 1 {
		return E.New("featureprint: Conv needs an input, weights and one output")
	}
	in, from, err := c.tensor(n.inputs[0])
	if err != nil {
		return err
	}
	w, err := c.weight(n.inputs[1])
	if err != nil {
		return err
	}
	if len(w.dims) != 4 {
		return E.New("featureprint: only 2-D Conv is supported")
	}
	// Conv attributes (Operators.md, Conv): pads are [top, left, bottom, right].
	kernel := [2]int{int(w.dims[2]), int(w.dims[3])}
	groups := 1
	if value, found := n.attributes["group"]; found {
		groups = int(value.i)
	}
	strides := c.ints(n, "strides", 1, 1)
	pads := c.ints(n, "pads", 0, 0, 0, 0)
	dilations := c.ints(n, "dilations", 1, 1)
	shapeOfKernel := c.ints(n, "kernel_shape", w.dims[2:]...)
	if autoPad, found := n.attributes["auto_pad"]; found && string(autoPad.s) != "NOTSET" && string(autoPad.s) != "VALID" {
		return E.New("featureprint: Conv auto_pad ", string(autoPad.s), " is not supported")
	}
	if len(strides) != 2 || len(pads) != 4 || len(dilations) != 2 || dilations[0] != 1 || dilations[1] != 1 ||
		!slices.Equal(shapeOfKernel, w.dims[2:]) || strides[0] < 1 || strides[1] < 1 || slices.Min(pads) < 0 {
		return E.New("featureprint: Conv ", n.outputs[0], " has unsupported attributes")
	}
	outputs := int(w.dims[0])
	if groups < 1 || from.channels != int(w.dims[1])*groups || outputs%groups != 0 {
		return E.New("featureprint: Conv ", n.outputs[0], " does not fit its input")
	}
	bias := make([]float32, outputs)
	if len(n.inputs) > 2 && n.inputs[2] != "" {
		b, err := c.weight(n.inputs[2])
		if err != nil {
			return err
		}
		if len(b.floats) != outputs {
			return E.New("featureprint: the bias of Conv ", n.outputs[0], " does not fit")
		}
		copy(bias, b.floats)
	}
	to := shape{
		channels: outputs,
		height:   (from.height+int(pads[0]+pads[2])-kernel[0])/int(strides[0]) + 1,
		width:    (from.width+int(pads[1]+pads[3])-kernel[1])/int(strides[1]) + 1,
	}
	act, name, err := c.absorb(n.outputs[0])
	if err != nil {
		return err
	}
	out, err := c.define(name, to)
	if err != nil {
		return err
	}
	s := &convStep{
		in: in, out: out, from: from, to: to,
		kernel: kernel, stride: [2]int{int(strides[0]), int(strides[1])}, pad: [2]int{int(pads[0]), int(pads[1])},
		groups: groups, bias: bias, act: act,
	}
	switch {
	case groups == from.channels && groups == outputs:
		s.depthwise = true
		s.weights = w.floats
	default:
		s.direct = groups == 1 && kernel == [2]int{1, 1} && s.stride == [2]int{1, 1} && slices.Max(pads) == 0
		if !s.direct {
			c.scratch = max(c.scratch, from.channels/groups*kernel[0]*kernel[1]*to.area())
		}
		s.weights = packed(w.floats, groups, outputs/groups, from.channels/groups*kernel[0]*kernel[1])
	}
	c.steps = append(c.steps, s)
	return nil
}

func (c *compiler) activation(n node) error {
	if len(n.inputs) < 1 || len(n.outputs) != 1 {
		return E.New("featureprint: ", n.op, " needs an input and one output")
	}
	in, s, err := c.tensor(n.inputs[0])
	if err != nil {
		return err
	}
	a, err := c.bounds(n)
	if err != nil {
		return err
	}
	out, err := c.define(n.outputs[0], s)
	if err != nil {
		return err
	}
	c.steps = append(c.steps, &addStep{first: in, second: -1, out: out, act: a})
	return nil
}

// add takes two tensors of the same shape; Add broadcasts (Operators.md, Add), but the
// residual connections of the network never need it.
func (c *compiler) add(n node) error {
	if len(n.inputs) != 2 || len(n.outputs) != 1 {
		return E.New("featureprint: Add needs two inputs and one output")
	}
	first, s, err := c.tensor(n.inputs[0])
	if err != nil {
		return err
	}
	second, other, err := c.tensor(n.inputs[1])
	if err != nil {
		return err
	}
	if s != other {
		return E.New("featureprint: Add ", n.outputs[0], " would broadcast")
	}
	act, name, err := c.absorb(n.outputs[0])
	if err != nil {
		return err
	}
	out, err := c.define(name, s)
	if err != nil {
		return err
	}
	c.steps = append(c.steps, &addStep{first: first, second: second, out: out, act: act})
	return nil
}

func (c *compiler) globalAveragePool(n node) error {
	if len(n.inputs) != 1 {
		return E.New("featureprint: GlobalAveragePool needs one input")
	}
	in, s, err := c.tensor(n.inputs[0])
	if err != nil {
		return err
	}
	out, err := c.define(n.outputs[0], shape{channels: s.channels, height: 1, width: 1})
	if err != nil {
		return err
	}
	c.steps = append(c.steps, &poolStep{in: in, out: out, from: s})
	return nil
}

// allocate gives every tensor a buffer, reusing the buffers of tensors no later step reads.
func (n *network) allocate() {
	last := make([]int, len(n.sizes))
	for index := range last {
		last[index] = -1
	}
	for index, s := range n.steps {
		inputs, _ := s.uses()
		for _, id := range inputs {
			last[id] = index
		}
	}
	last[n.output] = len(n.steps)
	n.slots = make([]int, len(n.sizes))
	var free []int
	take := func(size int) int {
		fit, largest := -1, -1
		for index, slot := range free {
			if n.capacity[slot] >= size && (fit < 0 || n.capacity[slot] < n.capacity[free[fit]]) {
				fit = index
			}
			if largest < 0 || n.capacity[slot] > n.capacity[free[largest]] {
				largest = index
			}
		}
		chosen := fit
		if chosen < 0 {
			chosen = largest
		}
		if chosen < 0 {
			n.capacity = append(n.capacity, size)
			return len(n.capacity) - 1
		}
		slot := free[chosen]
		free = slices.Delete(free, chosen, chosen+1)
		n.capacity[slot] = max(n.capacity[slot], size)
		return slot
	}
	n.slots[n.input] = take(n.sizes[n.input])
	for index, s := range n.steps {
		inputs, output := s.uses()
		n.slots[output] = take(n.sizes[output])
		for position, id := range inputs {
			if last[id] == index && !slices.Contains(inputs[:position], id) {
				free = append(free, n.slots[id])
			}
		}
		if last[output] < index {
			free = append(free, n.slots[output])
		}
	}
}

// pooled runs the network on the input fill writes, a channels by height by width tensor,
// and answers the pooled features.
func (n *network) pooled(ctx context.Context, fill func(input []float32)) ([]float32, error) {
	w := n.pool.Get().(*workspace)
	defer n.pool.Put(w)
	fill(w.tensor(n.input))
	for _, s := range n.steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.run(w)
	}
	return slices.Clone(w.tensor(n.output)), nil
}
