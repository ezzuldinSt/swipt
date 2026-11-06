package feed

// What the stream says about itself: its parameter sets read to the colors of the SPS's VUI
// (ITU-T H.265 7.3.2.2, E.2.1).

// nalReader reads the RBSP of a NAL unit, dropping its emulation prevention bytes (H.265
// 7.4.2) as it goes, so a long NAL unit is read without copying it.
type nalReader struct {
	data    []byte
	next    int
	zeros   int
	current byte
	left    int
	bad     bool
}

// newNALReader reads the payload after the NAL unit's 2-byte header.
func newNALReader(nal []byte) *nalReader {
	if len(nal) < 2 {
		return &nalReader{bad: true}
	}
	return &nalReader{data: nal[2:]}
}

func (r *nalReader) load() bool {
	if r.next >= len(r.data) {
		return false
	}
	value := r.data[r.next]
	r.next++
	if r.zeros >= 2 && value == 3 {
		r.zeros = 0
		if r.next >= len(r.data) {
			return false
		}
		value = r.data[r.next]
		r.next++
	}
	if value == 0 {
		r.zeros++
	} else {
		r.zeros = 0
	}
	r.current, r.left = value, 8
	return true
}

func (r *nalReader) bit() uint32 {
	if r.left == 0 && !r.load() {
		r.bad = true
		return 0
	}
	r.left--
	return uint32(r.current>>r.left) & 1
}

func (r *nalReader) flag() bool {
	return r.bit() == 1
}

func (r *nalReader) bits(count int) uint32 {
	var value uint32
	for range count {
		value = value<<1 | r.bit()
	}
	return value
}

func (r *nalReader) skip(count int) {
	for range count {
		r.bit()
	}
}

// ue reads an unsigned Exp-Golomb code (9.2).
func (r *nalReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if r.bad || zeros >= 31 {
			r.bad = true
			return 0
		}
		zeros++
	}
	return 1<<zeros - 1 + r.bits(zeros)
}

// se reads a signed Exp-Golomb code (9.2.2).
func (r *nalReader) se() int32 {
	value := r.ue()
	if value&1 == 1 {
		return int32((value + 1) / 2)
	}
	return -int32(value / 2)
}

// colorDescription is what an SPS's VUI says of the stream's colors (E.3.1): the range when
// signal is set, and the H.273 code points of its primaries, transfer and matrix when described
// is set.
type colorDescription struct {
	signal, fullRange           bool
	described                   bool
	primaries, transfer, matrix uint8
}

// shortTermRPS is a short-term reference picture set (7.3.7), of which the slice header needs
// the pictures the current one uses.
type shortTermRPS struct {
	negative, positive []int32
	usedNegative       []bool
	usedPositive       []bool
}

func (s *shortTermRPS) used() int {
	count := 0
	for _, used := range s.usedNegative {
		if used {
			count++
		}
	}
	for _, used := range s.usedPositive {
		if used {
			count++
		}
	}
	return count
}

// spsInfo is the part of an SPS that colors and slice headers need.
type spsInfo struct {
	id                           uint32
	subLayers                    int
	chromaFormat                 int
	separatePlanes               bool
	width, height                int
	bitDepthLuma, bitDepthChroma int
	log2MaxPOC                   int
	log2MinCb, log2Ctb           int
	sao                          bool
	rps                          []shortTermRPS
	longTerm                     bool
	longTermUsed                 []bool
	temporalMVP                  bool
	color                        colorDescription
	// complete is set when the SPS was read to its extension flags; foreign is set when an
	// extension (multilayer, 3D, screen content) changes the slice header this reader knows.
	complete, foreign bool
}

func skipProfileTierLevel(r *nalReader, subLayers int) {
	r.skip(96)
	profile := make([]bool, subLayers)
	level := make([]bool, subLayers)
	for index := range subLayers {
		profile[index], level[index] = r.flag(), r.flag()
	}
	if subLayers > 0 {
		r.skip(2 * (8 - subLayers))
	}
	for index := range subLayers {
		if profile[index] {
			r.skip(88)
		}
		if level[index] {
			r.skip(8)
		}
	}
}

// skipScalingListData reads past scaling_list_data() (7.3.4).
func skipScalingListData(r *nalReader) {
	for size := 0; size < 4 && !r.bad; size++ {
		step := 1
		if size == 3 {
			step = 3
		}
		for matrix := 0; matrix < 6; matrix += step {
			if !r.flag() {
				r.ue()
				continue
			}
			if size > 1 {
				r.se()
			}
			for range min(64, 1<<(4+size<<1)) {
				r.se()
			}
		}
	}
}

// readShortTermRPS reads st_ref_pic_set(index) with the sets before it (7.3.7, 7.4.8).
func readShortTermRPS(r *nalReader, index, total int, sets []shortTermRPS) shortTermRPS {
	var out shortTermRPS
	if index == 0 || !r.flag() {
		negative, positive := int(r.ue()), int(r.ue())
		if negative > 16 || positive > 16 {
			r.bad = true
			return out
		}
		poc := int32(0)
		for range negative {
			poc -= int32(r.ue()) + 1
			out.negative, out.usedNegative = append(out.negative, poc), append(out.usedNegative, r.flag())
		}
		poc = 0
		for range positive {
			poc += int32(r.ue()) + 1
			out.positive, out.usedPositive = append(out.positive, poc), append(out.usedPositive, r.flag())
		}
		return out
	}
	deltaIndex := 1
	if index == total {
		deltaIndex = int(r.ue()) + 1
	}
	negativeSign := r.flag()
	delta := int32(r.ue()) + 1
	if negativeSign {
		delta = -delta
	}
	if index-deltaIndex < 0 || index-deltaIndex >= len(sets) {
		r.bad = true
		return out
	}
	ref := sets[index-deltaIndex]
	negatives, positives := len(ref.negative), len(ref.positive)
	used := make([]bool, negatives+positives+1)
	useDelta := make([]bool, negatives+positives+1)
	for j := range used {
		used[j] = r.flag()
		useDelta[j] = used[j] || r.flag()
	}
	for j := positives - 1; j >= 0; j-- {
		if poc := ref.positive[j] + delta; poc < 0 && useDelta[negatives+j] {
			out.negative, out.usedNegative = append(out.negative, poc), append(out.usedNegative, used[negatives+j])
		}
	}
	if delta < 0 && useDelta[negatives+positives] {
		out.negative, out.usedNegative = append(out.negative, delta), append(out.usedNegative, used[negatives+positives])
	}
	for j := range negatives {
		if poc := ref.negative[j] + delta; poc < 0 && useDelta[j] {
			out.negative, out.usedNegative = append(out.negative, poc), append(out.usedNegative, used[j])
		}
	}
	for j := negatives - 1; j >= 0; j-- {
		if poc := ref.negative[j] + delta; poc > 0 && useDelta[j] {
			out.positive, out.usedPositive = append(out.positive, poc), append(out.usedPositive, used[j])
		}
	}
	if delta > 0 && useDelta[negatives+positives] {
		out.positive, out.usedPositive = append(out.positive, delta), append(out.usedPositive, used[negatives+positives])
	}
	for j := range positives {
		if poc := ref.positive[j] + delta; poc > 0 && useDelta[negatives+j] {
			out.positive, out.usedPositive = append(out.positive, poc), append(out.usedPositive, used[negatives+j])
		}
	}
	return out
}

// skipHRD reads past hrd_parameters(1, subLayers - 1) (E.2.2).
func skipHRD(r *nalReader, subLayers int) {
	nal, vcl := r.flag(), r.flag()
	subPicture := false
	if nal || vcl {
		if subPicture = r.flag(); subPicture {
			r.skip(8 + 5 + 1 + 5)
		}
		r.skip(4 + 4)
		if subPicture {
			r.skip(4)
		}
		r.skip(5 + 5 + 5)
	}
	for range subLayers {
		// fixed_pic_rate_within_cvs_flag follows only a clear fixed_pic_rate_general_flag.
		fixed := r.flag()
		if !fixed {
			fixed = r.flag()
		}
		lowDelay := false
		if fixed {
			r.ue()
		} else {
			lowDelay = r.flag()
		}
		count := 1
		if !lowDelay {
			count = int(r.ue()) + 1
		}
		if count > 32 {
			r.bad = true
			return
		}
		for _, present := range []bool{nal, vcl} {
			if !present {
				continue
			}
			for range count {
				r.ue()
				r.ue()
				if subPicture {
					r.ue()
					r.ue()
				}
				r.skip(1)
			}
		}
	}
}

// readVUI reads vui_parameters() (E.2.1), keeping the colors.
func readVUI(r *nalReader, s *spsInfo) {
	if r.flag() { // aspect_ratio_info_present_flag
		if r.bits(8) == 255 {
			r.skip(32)
		}
	}
	if r.flag() { // overscan_info_present_flag
		r.skip(1)
	}
	if r.flag() { // video_signal_type_present_flag
		r.skip(3) // video_format
		s.color.signal, s.color.fullRange = true, r.flag()
		if r.flag() {
			s.color.described = true
			s.color.primaries, s.color.transfer, s.color.matrix = uint8(r.bits(8)), uint8(r.bits(8)), uint8(r.bits(8))
		}
	}
	if r.bad {
		s.color = colorDescription{}
		return
	}
	if r.flag() { // chroma_loc_info_present_flag
		r.ue()
		r.ue()
	}
	r.skip(3)     // neutral_chroma_indication_flag, field_seq_flag, frame_field_info_present_flag
	if r.flag() { // default_display_window_flag
		for range 4 {
			r.ue()
		}
	}
	if r.flag() { // vui_timing_info_present_flag
		r.skip(64)
		if r.flag() {
			r.ue()
		}
		if r.flag() {
			skipHRD(r, s.subLayers)
		}
	}
	if r.flag() { // bitstream_restriction_flag
		r.skip(3)
		for range 5 {
			r.ue()
		}
	}
}

// readSPS reads seq_parameter_set_rbsp() (7.3.2.2) as far as it goes; false when even the
// colors are out of reach.
func readSPS(nal []byte) (*spsInfo, bool) {
	if len(nal) < 3 || NALType(nal) != nalSPS {
		return nil, false
	}
	r := newNALReader(nal)
	s := &spsInfo{}
	r.skip(4)
	s.subLayers = int(r.bits(3)) + 1
	r.skip(1)
	skipProfileTierLevel(r, s.subLayers-1)
	s.id = r.ue()
	s.chromaFormat = int(r.ue())
	if s.chromaFormat == 3 {
		s.separatePlanes = r.flag()
	}
	s.width, s.height = int(r.ue()), int(r.ue())
	if r.flag() {
		for range 4 {
			r.ue()
		}
	}
	s.bitDepthLuma, s.bitDepthChroma = int(r.ue())+8, int(r.ue())+8
	s.log2MaxPOC = int(r.ue()) + 4
	first := s.subLayers - 1
	if r.flag() {
		first = 0
	}
	for range s.subLayers - first {
		r.ue()
		r.ue()
		r.ue()
	}
	s.log2MinCb = int(r.ue()) + 3
	s.log2Ctb = s.log2MinCb + int(r.ue())
	for range 4 {
		r.ue()
	}
	if scaling := r.flag(); scaling && r.flag() { // sps_scaling_list_data_present_flag
		skipScalingListData(r)
	}
	r.skip(1) // amp_enabled_flag
	s.sao = r.flag()
	if r.flag() { // pcm_enabled_flag
		r.skip(8)
		r.ue()
		r.ue()
		r.skip(1)
	}
	count := int(r.ue())
	if r.bad || count > 64 || s.width <= 0 || s.height <= 0 || s.log2Ctb > 6 || s.log2MinCb > s.log2Ctb || s.chromaFormat > 3 {
		return nil, false
	}
	for index := range count {
		s.rps = append(s.rps, readShortTermRPS(r, index, count, s.rps))
	}
	if s.longTerm = r.flag(); s.longTerm {
		pictures := int(r.ue())
		if pictures > 32 {
			return nil, false
		}
		for range pictures {
			r.skip(s.log2MaxPOC)
			s.longTermUsed = append(s.longTermUsed, r.flag())
		}
	}
	s.temporalMVP = r.flag()
	r.skip(1) // strong_intra_smoothing_enabled_flag
	if r.bad {
		return nil, false
	}
	if r.flag() { // vui_parameters_present_flag
		readVUI(r, s)
	}
	if r.flag() { // sps_extension_present_flag
		_ = r.flag() // sps_range_extension_flag: nothing a slice header reads
		multilayer, threeD, screenContent := r.flag(), r.flag(), r.flag()
		s.foreign = multilayer || threeD || screenContent
	}
	s.complete = !r.bad
	return s, true
}
