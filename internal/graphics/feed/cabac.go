package feed

// The slice data, read far enough to tell whether every coding unit of a slice is skipped
// (ITU-T H.265 7.3.8, 9.3): the walk decodes SAO, the coding quadtree and the skipped coding
// units, and stops at the first coding unit that is not skipped. It reaches the end of a
// skipped slice only if it read every bin right, so it answers walkSkipped only then.

// rangeTabLPS is table 9-52, by pStateIdx and qRangeIdx.
var rangeTabLPS = [64][4]uint8{
	{128, 176, 208, 240},
	{128, 167, 197, 227},
	{128, 158, 187, 216},
	{123, 150, 178, 205},
	{116, 142, 169, 195},
	{111, 135, 160, 185},
	{105, 128, 152, 175},
	{100, 122, 144, 166},
	{95, 116, 137, 158},
	{90, 110, 130, 150},
	{85, 104, 123, 142},
	{81, 99, 117, 135},
	{77, 94, 111, 128},
	{73, 89, 105, 122},
	{69, 85, 100, 116},
	{66, 80, 95, 110},
	{62, 76, 90, 104},
	{59, 72, 86, 99},
	{56, 69, 81, 94},
	{53, 65, 77, 89},
	{51, 62, 73, 85},
	{48, 59, 69, 80},
	{46, 56, 66, 76},
	{43, 53, 63, 72},
	{41, 50, 59, 69},
	{39, 48, 56, 65},
	{37, 45, 54, 62},
	{35, 43, 51, 59},
	{33, 41, 48, 56},
	{32, 39, 46, 53},
	{30, 37, 43, 50},
	{29, 35, 41, 48},
	{27, 33, 39, 45},
	{26, 31, 37, 43},
	{24, 30, 35, 41},
	{23, 28, 33, 39},
	{22, 27, 32, 37},
	{21, 26, 30, 35},
	{20, 24, 29, 33},
	{19, 23, 27, 31},
	{18, 22, 26, 30},
	{17, 21, 25, 28},
	{16, 20, 23, 27},
	{15, 19, 22, 25},
	{14, 18, 21, 24},
	{14, 17, 20, 23},
	{13, 16, 19, 22},
	{12, 15, 18, 21},
	{12, 14, 17, 20},
	{11, 14, 16, 19},
	{11, 13, 15, 18},
	{10, 12, 15, 17},
	{10, 12, 14, 16},
	{9, 11, 13, 15},
	{9, 11, 12, 14},
	{8, 10, 12, 14},
	{8, 9, 11, 13},
	{7, 9, 11, 12},
	{7, 9, 10, 12},
	{7, 8, 10, 11},
	{6, 8, 9, 11},
	{6, 7, 9, 10},
	{6, 7, 8, 9},
	{2, 2, 2, 2},
}

// transIdxLPS is table 9-53; the MPS transition is pStateIdx + 1, at most 62.
var transIdxLPS = [64]uint8{
	0, 0, 1, 2, 2, 4, 4, 5, 6, 7, 8, 9, 9, 11, 11, 12,
	13, 13, 15, 15, 16, 16, 18, 18, 19, 19, 21, 21, 22, 22, 23, 24,
	24, 25, 26, 26, 27, 27, 28, 29, 29, 30, 30, 30, 31, 32, 32, 33,
	33, 33, 34, 34, 35, 35, 35, 36, 36, 36, 37, 37, 37, 38, 38, 63,
}

// cabacContext is a context variable: pStateIdx and valMps.
type cabacContext struct {
	state, mps uint8
}

// init initializes the context from its initValue at the slice's QP (9.3.2.2).
func (c *cabacContext) init(value, qp int) {
	slope, offset := value>>4, value&15
	m, n := slope*5-45, offset<<3-16
	state := min(max((m*min(max(qp, 0), 51))>>4+n, 1), 126)
	if state <= 63 {
		c.state, c.mps = uint8(63-state), 0
	} else {
		c.state, c.mps = uint8(state-64), 1
	}
}

// cabacDecoder is the arithmetic decoding engine (9.3.4.3).
type cabacDecoder struct {
	r      *nalReader
	rng    uint32
	offset uint32
}

func (d *cabacDecoder) start() {
	d.rng, d.offset = 510, d.r.bits(9)
}

func (d *cabacDecoder) renormalize() {
	for d.rng < 256 {
		d.rng <<= 1
		d.offset = d.offset<<1 | d.r.bit()
	}
}

func (d *cabacDecoder) decision(c *cabacContext) uint32 {
	lps := uint32(rangeTabLPS[c.state][(d.rng>>6)&3])
	d.rng -= lps
	var bin uint32
	if d.offset >= d.rng {
		bin = uint32(1 - c.mps)
		d.offset -= d.rng
		d.rng = lps
		if c.state == 0 {
			c.mps = 1 - c.mps
		}
		c.state = transIdxLPS[c.state]
	} else {
		bin = uint32(c.mps)
		c.state = min(c.state+1, 62)
	}
	d.renormalize()
	return bin
}

func (d *cabacDecoder) bypass() uint32 {
	d.offset = d.offset<<1 | d.r.bit()
	if d.offset >= d.rng {
		d.offset -= d.rng
		return 1
	}
	return 0
}

// terminate decodes end_of_slice_segment_flag or end_of_subset_one_bit. After a 1, the engine
// has read the bit that ends the substream: rbsp_stop_one_bit or alignment_bit_equal_to_one.
func (d *cabacDecoder) terminate() uint32 {
	d.rng -= 2
	if d.offset >= d.rng {
		return 1
	}
	d.renormalize()
	return 0
}

// walkContexts are the context variables the walk reads.
type walkContexts struct {
	saoMerge, saoType cabacContext
	split             [3]cabacContext
	transquantBypass  cabacContext
	skip              [3]cabacContext
	mergeIndex        cabacContext
}

// walkInitValues are their initValue for initType 0, 1 and 2 (tables 9-5 to 9-37); intra
// slices, initType 0, have no skip or merge flags.
var walkInitValues = [3][10]int{
	// sao_merge, sao_type_idx, split_cu_flag ×3, cu_transquant_bypass_flag, cu_skip_flag ×3, merge_idx
	{153, 200, 139, 141, 157, 154, 154, 154, 154, 154},
	{153, 185, 107, 139, 126, 154, 197, 185, 201, 122},
	{153, 160, 107, 139, 126, 154, 197, 185, 201, 137},
}

func (c *walkContexts) init(initType, qp int) {
	values := walkInitValues[initType]
	for index, context := range []*cabacContext{
		&c.saoMerge, &c.saoType, &c.split[0], &c.split[1], &c.split[2], &c.transquantBypass,
		&c.skip[0], &c.skip[1], &c.skip[2], &c.mergeIndex,
	} {
		context.init(values[index], qp)
	}
}

// walkResult is what the walk found in a slice.
type walkResult uint8

const (
	// walkUnknown: the slice could not be read to its end, or it uses what the walk does not read.
	walkUnknown walkResult = iota
	// walkCoded: an intra slice, or a coding unit that is not skipped.
	walkCoded
	// walkSkipped: every coding unit is skipped.
	walkSkipped
)

// skipWalker walks slices; it keeps the scan tables and the depths and skip flags of the
// picture's minimum coding blocks between them.
type skipWalker struct {
	sps *spsInfo
	pps *ppsInfo
	// The CTB scans of 6.5.1 and the tile of each CTB in tile scan.
	rsToTs, tsToRs, tileID []int
	widthCtbs              int
	// depth and skipped hold cqtDepth and cu_skip_flag of each minimum coding block.
	depth        []uint8
	skipped      []bool
	widthMinCbs  int
	h            sliceHeader
	d            cabacDecoder
	contexts     walkContexts
	saved        walkContexts
	sliceStartTs int
	currentTs    int
	// coded is where the walk met the first coding unit that is not skipped.
	coded [2]int
	// offsets is set when the slice walked last gave a CTB sample adaptive offsets, which change
	// its pixels even where every unit is skipped.
	offsets bool
}

// prepare makes the scan tables of the parameter sets; false when the tiles do not fit.
func (w *skipWalker) prepare(s *spsInfo, p *ppsInfo) bool {
	if w.sps == s && w.pps == p {
		return true
	}
	w.sps, w.pps = nil, nil
	width, height := s.ctbs()
	columns, rows := []int{width}, []int{height}
	if p.tiles {
		var ok bool
		if columns, ok = tileSizes(width, p.tileColumns, p.uniformTiles, p.columnWidths); !ok {
			return false
		}
		if rows, ok = tileSizes(height, p.tileRows, p.uniformTiles, p.rowHeights); !ok {
			return false
		}
	}
	count := width * height
	w.rsToTs, w.tsToRs, w.tileID = make([]int, count), make([]int, count), make([]int, count)
	columnStart, rowStart := boundaries(columns), boundaries(rows)
	for rs := range count {
		x, y := rs%width, rs/width
		tileX, tileY := tileOf(columnStart, x), tileOf(rowStart, y)
		ts := 0
		for index := range tileX {
			ts += rows[tileY] * columns[index]
		}
		for index := range tileY {
			ts += width * rows[index]
		}
		ts += (y-rowStart[tileY])*columns[tileX] + x - columnStart[tileX]
		w.rsToTs[rs], w.tsToRs[ts] = ts, rs
		w.tileID[ts] = tileY*len(columns) + tileX
	}
	w.widthCtbs = width
	minCb := 1 << s.log2MinCb
	w.widthMinCbs = (s.width + minCb - 1) / minCb
	cells := w.widthMinCbs * ((s.height + minCb - 1) / minCb)
	if cap(w.depth) < cells {
		w.depth, w.skipped = make([]uint8, cells), make([]bool, cells)
	}
	w.depth, w.skipped = w.depth[:cells], w.skipped[:cells]
	w.sps, w.pps = s, p
	return true
}

// tileSizes are the widths or heights of the tile columns or rows in CTBs (6.5.1).
func tileSizes(total, count int, uniform bool, given []int) ([]int, bool) {
	if count < 1 || count > total {
		return nil, false
	}
	sizes := make([]int, count)
	if uniform {
		for index := range sizes {
			sizes[index] = (index+1)*total/count - index*total/count
		}
		return sizes, true
	}
	if len(given) != count-1 {
		return nil, false
	}
	last := total
	for index, size := range given {
		sizes[index] = size
		last -= size
	}
	sizes[count-1] = last
	return sizes, last > 0
}

func boundaries(sizes []int) []int {
	starts := make([]int, len(sizes)+1)
	for index, size := range sizes {
		starts[index+1] = starts[index] + size
	}
	return starts
}

func tileOf(starts []int, at int) int {
	for index := len(starts) - 2; index > 0; index-- {
		if at >= starts[index] {
			return index
		}
	}
	return 0
}

// walk reads a slice's data from r, at its first bit.
func (w *skipWalker) walk(r *nalReader, h sliceHeader) walkResult {
	if h.kind == sliceI {
		return walkCoded
	}
	if h.dependent || !w.prepare(h.sps, h.pps) {
		return walkUnknown
	}
	w.h = h
	w.offsets = false
	p := h.pps
	initType := 1
	if h.kind == sliceB {
		initType = 2
	}
	if h.cabacInit {
		initType = 3 - initType
	}
	width, total := w.widthCtbs, len(w.tsToRs)
	ts := w.rsToTs[h.address]
	w.sliceStartTs = ts
	w.contexts.init(initType, h.qp)
	w.d = cabacDecoder{r: r}
	w.d.start()
	for {
		rs := w.tsToRs[ts]
		x, y := rs%width, rs/width
		w.currentTs = ts
		if ts != w.sliceStartTs {
			switch {
			case w.tileID[ts] != w.tileID[ts-1]:
				w.contexts.init(initType, h.qp)
			case p.wavefront && (x == 0 || w.tileID[ts] != w.tileID[w.rsToTs[rs-1]]):
				// The CTB above and to the right decides (9.3.1): its state after the second
				// CTB of its row, or the initial one.
				if x+1 < width && y > 0 && w.ctbAvailable(rs-width+1) {
					w.contexts = w.saved
				} else {
					w.contexts.init(initType, h.qp)
				}
			}
		}
		if result := w.ctu(x, y, rs, ts); result != walkSkipped || r.bad {
			if r.bad {
				return walkUnknown
			}
			return result
		}
		if p.wavefront && (x == 1 && width > 1 || rs > 1 && x > 1 && w.tileID[ts] != w.tileID[w.rsToTs[rs-2]]) {
			w.saved = w.contexts
		}
		if w.d.terminate() == 1 {
			if !r.restIsZero() {
				return walkUnknown
			}
			return walkSkipped
		}
		ts++
		if ts >= total {
			return walkUnknown
		}
		next := w.tsToRs[ts]
		if p.tiles && w.tileID[ts] != w.tileID[ts-1] || p.wavefront && (next%width == 0 || w.tileID[ts] != w.tileID[w.rsToTs[next-1]]) {
			if w.d.terminate() != 1 || r.left > 0 && r.current&(1<<r.left-1) != 0 {
				return walkUnknown
			}
			r.align()
			w.d.start()
		}
	}
}

// ctbAvailable tells whether a CTB before the current one is in its slice and tile (6.4.1).
func (w *skipWalker) ctbAvailable(rs int) bool {
	ts := w.rsToTs[rs]
	return ts >= w.sliceStartTs && ts <= w.currentTs && w.tileID[ts] == w.tileID[w.currentTs]
}

// available tells whether a luma location left of or above the current block is decoded in
// the slice and tile.
func (w *skipWalker) available(x, y int) bool {
	s := w.sps
	if x < 0 || y < 0 || x >= s.width || y >= s.height {
		return false
	}
	return w.ctbAvailable((y>>s.log2Ctb)*w.widthCtbs + x>>s.log2Ctb)
}

func (w *skipWalker) cell(x, y int) int {
	return (y>>w.sps.log2MinCb)*w.widthMinCbs + x>>w.sps.log2MinCb
}

// ctu reads coding_tree_unit() (7.3.8.2).
func (w *skipWalker) ctu(x, y, rs, ts int) walkResult {
	if w.h.saoLuma || w.h.saoChroma {
		w.sao(x, y, rs, ts)
	}
	return w.quadtree(x<<w.sps.log2Ctb, y<<w.sps.log2Ctb, w.sps.log2Ctb, 0)
}

// sao reads sao() (7.3.8.3).
func (w *skipWalker) sao(x, y, rs, ts int) {
	h, s, d := &w.h, w.sps, &w.d
	sliceAddress := h.address
	merged := false
	if x > 0 && rs > sliceAddress && w.tileID[ts] == w.tileID[w.rsToTs[rs-1]] {
		merged = d.decision(&w.contexts.saoMerge) == 1
	}
	if !merged && y > 0 && rs-w.widthCtbs >= sliceAddress && w.tileID[ts] == w.tileID[w.rsToTs[rs-w.widthCtbs]] {
		merged = d.decision(&w.contexts.saoMerge) == 1
	}
	if merged {
		return
	}
	components := 3
	if s.chromaArrayType() == 0 {
		components = 1
	}
	chromaType := 0
	for component := range components {
		if component == 0 && !h.saoLuma || component > 0 && !h.saoChroma {
			continue
		}
		kind := chromaType
		if component < 2 {
			// sao_type_idx: TR with cMax 2, its first bin in context and the second bypassed.
			kind = 0
			if d.decision(&w.contexts.saoType) == 1 {
				kind = 1 + int(d.bypass())
			}
			if component == 1 {
				chromaType = kind
			}
		}
		if kind == 0 {
			continue
		}
		w.offsets = true
		depth := s.bitDepthLuma
		if component > 0 {
			depth = s.bitDepthChroma
		}
		largest := 1<<(min(depth, 10)-5) - 1
		var offsets [4]int
		for index := range offsets {
			for offsets[index] < largest && d.bypass() == 1 {
				offsets[index]++
			}
		}
		if kind == 1 {
			// Band offset: the signs of the nonzero offsets, then sao_band_position.
			for _, offset := range offsets {
				if offset != 0 {
					d.bypass()
				}
			}
			for range 5 {
				d.bypass()
			}
		} else if component < 2 {
			d.bypass() // sao_eo_class
			d.bypass()
		}
	}
}

// quadtree reads coding_quadtree() (7.3.8.4).
func (w *skipWalker) quadtree(x0, y0, log2Size, depth int) walkResult {
	s := w.sps
	size := 1 << log2Size
	split := log2Size > s.log2MinCb
	if x0+size <= s.width && y0+size <= s.height && log2Size > s.log2MinCb {
		increment := 0
		if w.available(x0-1, y0) && int(w.depth[w.cell(x0-1, y0)]) > depth {
			increment++
		}
		if w.available(x0, y0-1) && int(w.depth[w.cell(x0, y0-1)]) > depth {
			increment++
		}
		split = w.d.decision(&w.contexts.split[increment]) == 1
	}
	if !split {
		return w.codingUnit(x0, y0, log2Size, depth)
	}
	half := size >> 1
	for _, corner := range [4][2]int{{0, 0}, {half, 0}, {0, half}, {half, half}} {
		x1, y1 := x0+corner[0], y0+corner[1]
		if x1 >= s.width || y1 >= s.height {
			continue
		}
		if result := w.quadtree(x1, y1, log2Size-1, depth+1); result != walkSkipped {
			return result
		}
	}
	return walkSkipped
}

// codingUnit reads coding_unit() (7.3.8.5) as far as cu_skip_flag, and a skipped unit's
// merge_idx.
func (w *skipWalker) codingUnit(x0, y0, log2Size, depth int) walkResult {
	d := &w.d
	if w.pps.transquantBypass {
		d.decision(&w.contexts.transquantBypass)
	}
	increment := 0
	if w.available(x0-1, y0) && w.skipped[w.cell(x0-1, y0)] {
		increment++
	}
	if w.available(x0, y0-1) && w.skipped[w.cell(x0, y0-1)] {
		increment++
	}
	if d.decision(&w.contexts.skip[increment]) == 0 {
		w.coded = [2]int{x0, y0}
		return walkCoded
	}
	if candidates := w.h.maxMergeCandidates; candidates > 1 && d.decision(&w.contexts.mergeIndex) == 1 {
		for index := 1; index < candidates-1 && d.bypass() == 1; index++ {
		}
	}
	s := w.sps
	minCb := 1 << s.log2MinCb
	right, bottom := min(x0+1<<log2Size, s.width), min(y0+1<<log2Size, s.height)
	for y := y0; y < bottom; y += minCb {
		for x := x0; x < right; x += minCb {
			cell := w.cell(x, y)
			w.depth[cell], w.skipped[cell] = uint8(depth), true
		}
	}
	return walkSkipped
}
