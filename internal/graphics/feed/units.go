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
}

// Changed tells whether the unit may change the picture: it codes something, it weights its
// references, or it could not be read. A unit whose coding units are all skipped can still
// move blocks by the motion vectors they merge, as x265 moves a square across the test
// fixture with skipped slices, so unchanged means nothing was coded, not that nothing moved.
func (u UnitInfo) Changed() bool {
	return u.Skip != SkipAll || u.Weighted
}

// Changes sums access units: how many, their bytes, and whether any may change the picture.
type Changes struct {
	Units, Bytes int64
	Changed      bool
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
// including it, and the count at the last unit that may change the picture.
type unitTotals struct {
	units, bytes, changedAt int64
}

// since sums the units after earlier, up to and including these.
func (t unitTotals) since(earlier unitTotals) Changes {
	return Changes{Units: t.units - earlier.units, Bytes: t.bytes - earlier.bytes, Changed: t.changedAt > earlier.units}
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
	if first || unit.Info.Changed() {
		f.changedAt.Store(units)
	}
	unit.totals = unitTotals{units: units, bytes: bytes, changedAt: f.changedAt.Load()}
	info := unit.Info
	f.lastUnit.Store(&info)
}

// unitInspector reads the parameter sets and slices of a stream's units.
type unitInspector struct {
	sps    map[uint32]*spsInfo
	pps    map[uint32]*ppsInfo
	walker skipWalker
}

func newUnitInspector() *unitInspector {
	return &unitInspector{sps: make(map[uint32]*spsInfo), pps: make(map[uint32]*ppsInfo)}
}

// inspect reads a unit: its parameter sets first, then each slice's header and, while nothing
// coded has been found, its data.
func (i *unitInspector) inspect(unit *AccessUnit) UnitInfo {
	var info UnitInfo
	found, coded, unknown, qp := false, false, unit.Damaged, false
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
			if coded || unknown {
				continue
			}
			switch i.walker.walk(r, header) {
			case walkCoded:
				coded = true
			case walkUnknown:
				unknown = true
			}
		}
	}
	switch {
	case coded:
		info.Skip = SkipNone
	case found && !unknown:
		info.Skip = SkipAll
	}
	return info
}
