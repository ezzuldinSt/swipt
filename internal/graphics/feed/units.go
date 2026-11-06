package feed

// SkipState tells what the slices of an access unit code.
type SkipState uint8

const (
	// SkipUnknown: a slice could not be read to its end, the unit was damaged or had no slice.
	SkipUnknown SkipState = iota
	// SkipNone: a slice codes something: it is intra, or a coding unit of it is not skipped.
	SkipNone
	// SkipAll: every coding unit of every slice is skipped, so the picture repeats its
	// reference; on a screen that means nothing changed, though a skipped unit may take a
	// merged motion vector and move.
	SkipAll
)

func (s SkipState) String() string {
	switch s {
	case SkipNone:
		return "coded"
	case SkipAll:
		return "skipped"
	}
	return "unknown"
}

// UnitInfo is what an access unit's own bytes say of it.
type UnitInfo struct {
	// Bytes counts its NAL units' bytes.
	Bytes int
	// Slices counts its slice segments; Intra, Predicted and Bidirectional count the ones
	// whose header was read, by slice type.
	Slices                          int
	Intra, Predicted, Bidirectional int
	// MinQP and MaxQP are the range of its slices' SliceQpY, 0 when no header was read.
	MinQP, MaxQP int
	Skip         SkipState
	// Weighted is set when a slice weights its references, as for a fade: then even skipped
	// units change the colors, as x265 turns the fixture's blue screen gray with them.
	Weighted bool
	// TemporalMVP is set when a slice predicts motion from the collocated picture, and
	// References is the most active references a slice has: with either, skipped units can
	// still move blocks or copy from an older picture.
	TemporalMVP bool
	References  int
	// Repeats is set when the unit's picture is the one before it, sample for sample: every
	// coding unit is skipped in P slices that predict from the picture before only, without
	// temporal motion vectors, weights or sample adaptive offsets. Merging then only finds zero
	// motion vectors, nothing is deblocked, and the picture copies its reference.
	Repeats bool
}

// Changed tells whether the unit may change the picture: it codes something, it weights its
// references, or it could not be read. A unit whose coding units are all skipped can still
// move blocks by the motion vectors they merge, as x265 moves a square across the test
// fixture with skipped slices, so unchanged means nothing was coded, not that nothing moved;
// Repeats tells that nothing moved either.
func (u UnitInfo) Changed() bool {
	return u.Skip != SkipAll || u.Weighted
}

// Changes sums access units: how many, their bytes, whether any may change the picture, and
// whether all of them repeat the picture before them, so the last one's picture is the one
// before the first.
type Changes struct {
	Units, Bytes int64
	Changed      bool
	Repeats      bool
}

// FrameInfo is what the feed knows of a decoded frame.
type FrameInfo struct {
	// Unit is what the access unit the picture decoded from says of itself.
	Unit UnitInfo
	// SincePrevious sums the units after the previous decoded frame's, up to and including this
	// one's: a unit that gave no picture counts toward the next frame, and the first unit of a
	// stream counts as changed.
	SincePrevious Changes
	// Arrival is when the unit's first packet reached the feed and Decoded when its picture came
	// out of the decoder, in milliseconds since the epoch on the precise clock.
	Arrival, Decoded float64
}

// unitTotals are a feed's running counts at an access unit: units and bytes up to and
// including it, and the counts at the last unit that may change the picture and at the last
// one that does not repeat it.
type unitTotals struct {
	units, bytes, changedAt, movedAt int64
}

// since sums the units after earlier, up to and including these.
func (t unitTotals) since(earlier unitTotals) Changes {
	return Changes{Units: t.units - earlier.units, Bytes: t.bytes - earlier.bytes, Changed: t.changedAt > earlier.units, Repeats: t.movedAt <= earlier.units}
}

// count reads a unit and adds it to the feed's running counts, on the decoding goroutine; the
// first unit of a stream counts as changed.
func (f *Feed) count(inspector *unitInspector, unit *AccessUnit, first bool) {
	unit.Info = inspector.inspect(unit)
	units := f.units.Add(1)
	bytes := f.unitBytes.Add(int64(unit.Info.Bytes))
	switch unit.Info.Skip {
	case SkipNone:
		f.coded.Add(1)
	case SkipAll:
		f.skipped.Add(1)
	default:
		f.unread.Add(1)
	}
	if unit.Info.Repeats {
		f.repeated.Add(1)
	}
	if first || unit.Info.Changed() {
		f.changedAt.Store(units)
	}
	if first || !unit.Info.Repeats {
		f.movedAt.Store(units)
	}
	unit.totals = unitTotals{units: units, bytes: bytes, changedAt: f.changedAt.Load(), movedAt: f.movedAt.Load()}
	info := unit.Info
	f.lastUnit.Store(&info)
}

// unitInspector reads the parameter sets and slices of a stream's units, and keeps the picture
// order count of the last one.
type unitInspector struct {
	sps    map[uint32]*spsInfo
	pps    map[uint32]*ppsInfo
	walker skipWalker
	// previousPOC is the slice_pic_order_cnt_lsb of the unit before, while hasPrevious.
	previousPOC int
	hasPrevious bool
}

func newUnitInspector() *unitInspector {
	return &unitInspector{sps: make(map[uint32]*spsInfo), pps: make(map[uint32]*ppsInfo)}
}

// inspect reads a unit: its parameter sets first, then each slice's header and, while nothing
// coded has been found, its data.
func (i *unitInspector) inspect(unit *AccessUnit) UnitInfo {
	var info UnitInfo
	found, coded, unknown, qp := false, false, unit.Damaged, false
	// repeats holds while every slice read predicts from the unit before only, plainly.
	repeats := i.hasPrevious
	poc, hasPOC := 0, false
	for _, nal := range unit.NALs {
		info.Bytes += len(nal)
		if len(nal) < 3 {
			continue
		}
		switch kind := NALType(nal); {
		case kind == nalSPS:
			if parsed, ok := readSPS(nal); ok {
				i.sps[parsed.id] = parsed
			}
		case kind == nalPPS:
			if parsed, ok := readPPS(nal); ok {
				i.pps[parsed.id] = parsed
			}
		case kind < 32:
			found = true
			info.Slices++
			r := newNALReader(nal)
			header, ok := readSliceHeader(r, kind, i.sps, i.pps)
			if !ok || header.dependent {
				unknown = true
				continue
			}
			switch header.kind {
			case sliceI:
				info.Intra++
			case sliceP:
				info.Predicted++
			default:
				info.Bidirectional++
			}
			if !qp {
				qp = true
				info.MinQP, info.MaxQP = header.qp, header.qp
			}
			info.MinQP, info.MaxQP = min(info.MinQP, header.qp), max(info.MaxQP, header.qp)
			info.Weighted = info.Weighted || header.weighted
			info.TemporalMVP = info.TemporalMVP || header.temporalMVP
			info.References = max(info.References, header.references)
			if !hasPOC {
				poc, hasPOC = header.pocLSB, true
			}
			maxPOC := 1 << header.sps.log2MaxPOC
			repeats = repeats && header.kind == sliceP && header.references == 1 && !header.temporalMVP && !header.weighted &&
				header.reference < 0 && ((header.pocLSB+int(header.reference))%maxPOC+maxPOC)%maxPOC == i.previousPOC
			if coded || unknown {
				continue
			}
			switch i.walker.walk(r, header) {
			case walkCoded:
				coded = true
			case walkUnknown:
				unknown = true
			}
			repeats = repeats && !i.walker.offsets
		}
	}
	switch {
	case coded:
		info.Skip = SkipNone
	case found && !unknown:
		info.Skip = SkipAll
	}
	info.Repeats = repeats && info.Skip == SkipAll
	i.previousPOC, i.hasPrevious = poc, hasPOC && !unknown
	return info
}
