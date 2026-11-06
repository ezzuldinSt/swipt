package feed

// The PPS and the slice segment header (ITU-T H.265 7.3.2.3, 7.3.6.1), read to the slice data.

// ppsInfo is the part of a PPS that slice headers and slice data need.
type ppsInfo struct {
	id, sps                  uint32
	dependentSlices          bool
	outputFlag               bool
	extraBits                int
	cabacInitPresent         bool
	refL0, refL1             int
	initQP                   int
	chromaQPOffsets          bool
	weightedP, weightedB     bool
	transquantBypass         bool
	tiles, wavefront         bool
	tileColumns, tileRows    int
	uniformTiles             bool
	columnWidths, rowHeights []int
	loopFilterAcrossSlices   bool
	deblockingOverride       bool
	deblockingDisabled       bool
	listsModification        bool
	headerExtension          bool
	chromaQPOffsetList       bool
	// complete is set when the PPS was read to its range extension; foreign is set when an
	// extension (multilayer, 3D, screen content) changes the slice header this reader knows.
	complete, foreign bool
}

// readPPS reads pic_parameter_set_rbsp() (7.3.2.3).
func readPPS(nal []byte) (*ppsInfo, bool) {
	if len(nal) < 3 || NALType(nal) != nalPPS {
		return nil, false
	}
	r := newNALReader(nal)
	p := &ppsInfo{}
	p.id, p.sps = r.ue(), r.ue()
	p.dependentSlices = r.flag()
	p.outputFlag = r.flag()
	p.extraBits = int(r.bits(3))
	r.skip(1) // sign_data_hiding_enabled_flag
	p.cabacInitPresent = r.flag()
	p.refL0, p.refL1 = int(r.ue())+1, int(r.ue())+1
	p.initQP = 26 + int(r.se())
	r.skip(1) // constrained_intra_pred_flag
	transformSkip := r.flag()
	if r.flag() { // cu_qp_delta_enabled_flag
		r.ue()
	}
	r.se()
	r.se()
	p.chromaQPOffsets = r.flag()
	p.weightedP, p.weightedB = r.flag(), r.flag()
	p.transquantBypass = r.flag()
	p.tiles, p.wavefront = r.flag(), r.flag()
	if p.tiles {
		p.tileColumns, p.tileRows = int(r.ue())+1, int(r.ue())+1
		if p.tileColumns > 64 || p.tileRows > 64 {
			return nil, false
		}
		if p.uniformTiles = r.flag(); !p.uniformTiles {
			for range p.tileColumns - 1 {
				p.columnWidths = append(p.columnWidths, int(r.ue())+1)
			}
			for range p.tileRows - 1 {
				p.rowHeights = append(p.rowHeights, int(r.ue())+1)
			}
		}
		r.skip(1) // loop_filter_across_tiles_enabled_flag
	}
	p.loopFilterAcrossSlices = r.flag()
	if r.flag() { // deblocking_filter_control_present_flag
		p.deblockingOverride = r.flag()
		if p.deblockingDisabled = r.flag(); !p.deblockingDisabled {
			r.se()
			r.se()
		}
	}
	if r.flag() { // pps_scaling_list_data_present_flag
		skipScalingListData(r)
	}
	p.listsModification = r.flag()
	r.ue() // log2_parallel_merge_level_minus2
	p.headerExtension = r.flag()
	if r.bad || p.initQP < -26 || p.initQP > 51 {
		return nil, false
	}
	if r.flag() { // pps_extension_present_flag
		rangeExtension := r.flag()
		multilayer, threeD, screenContent := r.flag(), r.flag(), r.flag()
		p.foreign = multilayer || threeD || screenContent
		r.skip(4)
		if rangeExtension {
			if transformSkip {
				r.ue()
			}
			r.skip(1) // cross_component_prediction_enabled_flag
			p.chromaQPOffsetList = r.flag()
		}
	}
	p.complete = !r.bad
	return p, true
}

// Slice types (table 7-7).
const (
	sliceB = 0
	sliceP = 1
	sliceI = 2
)

// sliceHeader is what slice_segment_header() (7.3.6.1) gives the slice data.
type sliceHeader struct {
	dependent          bool
	address            int
	kind               int
	qp                 int
	saoLuma, saoChroma bool
	cabacInit          bool
	maxMergeCandidates int
	// temporalMVP is slice_temporal_mvp_enabled_flag; references counts the active references
	// of the larger list; weighted is set when the prediction weights a reference.
	temporalMVP bool
	references  int
	weighted    bool
	// pocLSB is slice_pic_order_cnt_lsb, 0 for an IDR picture; reference is the POC delta of
	// the one picture the slice may predict from, 0 unless it uses exactly one, a short-term one.
	pocLSB    int
	reference int32
	sps       *spsInfo
	pps       *ppsInfo
}

// skipPredictionWeights reads past pred_weight_table() (7.3.6.3) of a single-layer stream,
// whose references never share the picture's order count, and tells whether it weights any
// reference.
func skipPredictionWeights(r *nalReader, s *spsInfo, kind, l0, l1 int) bool {
	weighted := false
	r.ue()
	chroma := s.chromaArrayType() != 0
	if chroma {
		r.se()
	}
	lists := []int{l0}
	if kind == sliceB {
		lists = append(lists, l1)
	}
	for _, count := range lists {
		luma := make([]bool, count)
		chromaWeights := make([]bool, count)
		for index := range count {
			luma[index] = r.flag()
		}
		if chroma {
			for index := range count {
				chromaWeights[index] = r.flag()
			}
		}
		for index := range count {
			if luma[index] {
				r.se()
				r.se()
			}
			if chromaWeights[index] {
				for range 4 {
					r.se()
				}
			}
			weighted = weighted || luma[index] || chromaWeights[index]
		}
	}
	return weighted
}

// readLongTermPictures reads the long-term pictures of a slice header and answers how many
// of them the picture uses.
func readLongTermPictures(r *nalReader, s *spsInfo) (int, bool) {
	fromSPS := 0
	if len(s.longTermUsed) > 0 {
		fromSPS = int(r.ue())
	}
	pictures := int(r.ue())
	if fromSPS+pictures > 32 {
		return 0, false
	}
	used := 0
	for index := range fromSPS + pictures {
		if index < fromSPS {
			picked := 0
			if len(s.longTermUsed) > 1 {
				picked = int(r.bits(ceilLog2(len(s.longTermUsed))))
			}
			if picked < len(s.longTermUsed) && s.longTermUsed[picked] {
				used++
			}
		} else {
			r.skip(s.log2MaxPOC)
			if r.flag() {
				used++
			}
		}
		if r.flag() { // delta_poc_msb_present_flag
			r.ue()
		}
	}
	return used, true
}

// readSliceHeader reads a slice segment header with the parameter sets it names and leaves r
// at its slice data; false when it cannot, as for an unknown PPS or an extension it does not
// read. A dependent slice segment's header stops after its address.
func readSliceHeader(r *nalReader, kind int, spsByID map[uint32]*spsInfo, ppsByID map[uint32]*ppsInfo) (sliceHeader, bool) {
	var h sliceHeader
	first := r.flag()
	if kind >= nalIRAPFirst && kind <= nalIRAPLast {
		r.skip(1) // no_output_of_prior_pics_flag
	}
	p := ppsByID[r.ue()]
	if p == nil || !p.complete || p.foreign {
		return h, false
	}
	s := spsByID[p.sps]
	if s == nil || !s.complete || s.foreign {
		return h, false
	}
	h.sps, h.pps = s, p
	if !first {
		h.dependent = p.dependentSlices && r.flag()
		width, height := s.ctbs()
		h.address = int(r.bits(ceilLog2(width * height)))
		if h.address >= width*height {
			return h, false
		}
	}
	if h.dependent {
		return h, !r.bad
	}
	r.skip(p.extraBits)
	h.kind = int(r.ue())
	if h.kind > sliceI {
		return h, false
	}
	if p.outputFlag {
		r.skip(1)
	}
	if s.separatePlanes {
		r.skip(2)
	}
	temporalMVP := false
	current := 0
	if kind != 19 && kind != 20 { // IDR_W_RADL, IDR_N_LP
		h.pocLSB = int(r.bits(s.log2MaxPOC))
		var set shortTermRPS
		if !r.flag() { // short_term_ref_pic_set_sps_flag
			set = readShortTermRPS(r, len(s.rps), len(s.rps), s.rps)
		} else {
			index := 0
			if len(s.rps) > 1 {
				index = int(r.bits(ceilLog2(len(s.rps))))
			}
			if index >= len(s.rps) {
				return h, false
			}
			set = s.rps[index]
		}
		current = set.used()
		longTerm := 0
		if s.longTerm {
			used, ok := readLongTermPictures(r, s)
			if !ok {
				return h, false
			}
			longTerm = used
			current += used
		}
		if current == 1 && longTerm == 0 {
			h.reference = set.usedDelta()
		}
		if s.temporalMVP {
			temporalMVP = r.flag()
			h.temporalMVP = temporalMVP
		}
	}
	if s.sao {
		h.saoLuma = r.flag()
		if s.chromaArrayType() != 0 {
			h.saoChroma = r.flag()
		}
	}
	if h.kind != sliceI {
		l0, l1 := p.refL0, p.refL1
		if r.flag() { // num_ref_idx_active_override_flag
			l0 = int(r.ue()) + 1
			if h.kind == sliceB {
				l1 = int(r.ue()) + 1
			}
		}
		h.references = l0
		if h.kind == sliceB {
			h.references = max(l0, l1)
		}
		if l0 > 16 || l1 > 16 {
			return h, false
		}
		if p.listsModification && current > 1 {
			bits := ceilLog2(current)
			if r.flag() {
				r.skip(l0 * bits)
			}
			if h.kind == sliceB && r.flag() {
				r.skip(l1 * bits)
			}
		}
		if h.kind == sliceB {
			r.skip(1) // mvd_l1_zero_flag
		}
		if p.cabacInitPresent {
			h.cabacInit = r.flag()
		}
		if temporalMVP {
			fromL0 := true
			if h.kind == sliceB {
				fromL0 = r.flag()
			}
			if fromL0 && l0 > 1 || !fromL0 && l1 > 1 {
				r.ue() // collocated_ref_idx
			}
		}
		if p.weightedP && h.kind == sliceP || p.weightedB && h.kind == sliceB {
			h.weighted = skipPredictionWeights(r, s, h.kind, l0, l1)
		}
		h.maxMergeCandidates = 5 - int(r.ue())
		if h.maxMergeCandidates < 1 || h.maxMergeCandidates > 5 {
			return h, false
		}
	}
	h.qp = p.initQP + int(r.se())
	if p.chromaQPOffsets {
		r.se()
		r.se()
	}
	if p.chromaQPOffsetList {
		r.skip(1)
	}
	deblockingDisabled := p.deblockingDisabled
	if p.deblockingOverride && r.flag() {
		if deblockingDisabled = r.flag(); !deblockingDisabled {
			r.se()
			r.se()
		}
	}
	if p.loopFilterAcrossSlices && (h.saoLuma || h.saoChroma || !deblockingDisabled) {
		r.skip(1)
	}
	if p.tiles || p.wavefront {
		if entries := int(r.ue()); entries > 0 {
			bits := int(r.ue()) + 1
			if bits > 32 || entries > 1<<16 {
				return h, false
			}
			r.skip(entries * bits)
		}
	}
	if p.headerExtension {
		length := int(r.ue())
		if length > 256 {
			return h, false
		}
		r.skip(8 * length)
	}
	// byte_alignment(): a one, then zeros to the byte.
	if r.bit() != 1 {
		return h, false
	}
	r.align()
	return h, !r.bad && h.qp >= -6*(s.bitDepthLuma-8) && h.qp <= 51
}
