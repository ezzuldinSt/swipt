package feed

import (
	"encoding/binary"
	"time"
)

// A tiled stream (device.Options.ScreenTiles) is one HEVC sequence whose pictures are the
// horizontal bands of the screen in turn: with two bands, the pictures of order count 0, 2,
// 4... are the top band and 1, 3, 5... the bottom one, all of the band's size with the same
// parameter sets, and a band's first picture after a keyframe request may be an intra
// picture of a trailing type that only decodes after the top band's IDR (iPhone18,2, iOS
// 27.0.1 24A446). Each band comes in its own RTP stream with SSRC base+band and its own
// sequence numbers, and every packet carries a decoding order number that counts the
// pictures of all bands together. So the bands decode in one decoder, in that order, and the
// pictures that share an RTP timestamp make one frame of the screen.
const (
	// maxBands is the most bands a frame is composed of; the device takes 2 and 4.
	maxBands = 4
	// mergeWait is how long, in milliseconds of arrival, the merger waits for a picture that
	// did not come before it gives it up for lost.
	mergeWait = 150.0
	// mergeSpan is how far a picture's decoding order number may be from the next one before
	// the merger takes it that the device numbers its pictures afresh.
	mergeSpan = 256
)

// bandMerger puts the access units of a tiled stream's bands in decoding order.
type bandMerger struct {
	base  uint32
	count int
	bands [maxBands]Depacketizer
	// latest is the decoding order number of each band's newest unit, while seen.
	latest [maxBands]uint16
	seen   [maxBands]bool

	started bool
	next    uint16
	waiting map[uint16]*AccessUnit
	scratch []*AccessUnit
	// lost is set when a picture was given up, so the next unit out is damaged.
	lost bool
}

func newBandMerger(count int, base uint32) *bandMerger {
	m := &bandMerger{base: base, count: min(max(count, 1), maxBands), waiting: make(map[uint16]*AccessUnit)}
	for band := range m.bands {
		m.bands[band].DecodingOrder = true
	}
	return m
}

// setReceived stamps the units that the next packets start (Depacketizer.received).
func (m *bandMerger) setReceived(received int64) {
	for band := range m.bands {
		m.bands[band].received = received
	}
}

// pushBatch parses the records of a delivered batch as Depacketizer.PushBatch does and appends
// the units that are next in decoding order to done.
func (m *bandMerger) pushBatch(batch []byte, done []*AccessUnit) []*AccessUnit {
	for offset := 0; offset+recordHeaderSize <= len(batch); {
		arrival := binary.BigEndian.Uint64(batch[offset:])
		rtcp := batch[offset+8] != 0
		length := int(binary.BigEndian.Uint32(batch[offset+9:]))
		offset += recordHeaderSize
		if length < 0 || offset+length > len(batch) {
			break
		}
		if !rtcp {
			done = m.push(batch[offset:offset+length], float64(arrival)/1e6, done)
		}
		offset += length
	}
	return done
}

// push adds an RTP packet of any band.
func (m *bandMerger) push(packet []byte, arrival float64, done []*AccessUnit) []*AccessUnit {
	if len(packet) < 12 {
		return done
	}
	band := int(binary.BigEndian.Uint32(packet[8:]) - m.base)
	if band < 0 || band >= m.count {
		return done
	}
	m.scratch = m.bands[band].Push(packet, arrival, m.scratch[:0])
	for _, unit := range m.scratch {
		unit.Band, unit.Bands = band, m.count
		done = m.add(unit, arrival, done)
	}
	clear(m.scratch)
	return done
}

// after tells whether decoding order number a comes after b.
func after(a, b uint16) bool {
	return int16(a-b) > 0
}

func (m *bandMerger) add(unit *AccessUnit, arrival float64, done []*AccessUnit) []*AccessUnit {
	number := unit.DecodingOrder
	if m.started {
		switch distance := int16(number - m.next); {
		case distance < -mergeSpan || distance > mergeSpan || len(m.waiting) > mergeSpan:
			// The device numbered its pictures afresh: start over at this one, which the
			// decoder takes as damage, so it waits for a keyframe.
			m.restart()
		case distance < 0:
			// It comes after a picture that was given up; the decoder went on without it.
			return done
		}
	}
	m.latest[unit.Band], m.seen[unit.Band] = number, true
	m.waiting[number] = unit
	if !m.started {
		// The first pictures may come in any band first: the sequence starts at the least
		// number once every band sent one, or mergeWait after the first.
		all := true
		for band := range m.count {
			all = all && m.seen[band]
		}
		oldest := m.oldestWaiting()
		if !all && arrival-m.waiting[oldest].Arrival < mergeWait {
			return done
		}
		m.started, m.next = true, oldest
	}
	for len(m.waiting) > 0 {
		if next, ok := m.waiting[m.next]; ok {
			delete(m.waiting, m.next)
			m.next++
			if m.lost {
				next.Damaged, m.lost = true, false
			}
			done = append(done, next)
			continue
		}
		// The next picture is lost once every band is past it, as each band's pictures come
		// in order, or once the oldest waiting one waited too long.
		passed := true
		for band := range m.count {
			passed = passed && m.seen[band] && after(m.latest[band], m.next)
		}
		oldest := m.oldestWaiting()
		if !passed && arrival-m.waiting[oldest].Arrival < mergeWait {
			break
		}
		m.next, m.lost = oldest, true
	}
	return done
}

func (m *bandMerger) restart() {
	m.started, m.lost = false, true
	clear(m.waiting)
	m.seen = [maxBands]bool{}
}

func (m *bandMerger) oldestWaiting() uint16 {
	first, found := uint16(0), false
	for number := range m.waiting {
		if !found || after(first, number) {
			first, found = number, true
		}
	}
	return first
}

// bandWait is how long a frame stays open after its newest band's picture when the device did
// not mark that band as the frame's last (AccessUnit.LastBand), before it is shown with the
// bands that came. The bands of a frame reached the PC within some 5 ms of each other with two
// bands and 11 ms with four (median, iPhone18,2, iOS 27.0.1 24A446 on Wi-Fi).
const bandWait = 12 * time.Millisecond

// composed is a frame of a tiled stream: its picture, the unit that stands for it, the first
// of its bands' units by arrival, and the bands' height.
type composed struct {
	picture    *picture
	unit       *AccessUnit
	bandHeight int
}

// composer makes the frames of a tiled stream from its bands' pictures, which come in
// decoding order, so a frame's bands come one after another in band order. A band that did
// not change is not sent and keeps its pixels from the frame before: base holds the screen as
// composed so far, and each frame is a copy of it once its bands are in.
type composer struct {
	base       *picture
	bandHeight int
	// written has a bit for each band written into base since it was made.
	written uint32

	// The frame being composed: whether one is, its timestamp, and its first and last units.
	open        bool
	timestamp   uint32
	first, last *AccessUnit
}

func (c *composer) close() {
	if c.base != nil {
		c.base.recycle()
		c.base = nil
	}
	c.open, c.written = false, 0
}

// add places a band's picture, which it takes over, into the screen of height rows (the whole
// bands when 0), and appends the frames it completes to done: the one being composed when the
// picture begins the next, and the picture's own when it is of the last band.
func (c *composer) add(p *picture, unit *AccessUnit, height int, done []composed) ([]composed, error) {
	defer p.recycle()
	if c.open && unit.RTPTimestamp != c.timestamp {
		done = c.finish(done)
	}
	if err := c.place(p, unit, height); err != nil {
		c.close()
		return done, err
	}
	if !c.open {
		c.open, c.timestamp, c.first = true, unit.RTPTimestamp, unit
	}
	if unit.Received != 0 && (c.first.Received == 0 || unit.Received < c.first.Received) {
		c.first = unit
	}
	c.last = unit
	if unit.Band == unit.Bands-1 || unit.EndKnown && unit.LastBand {
		done = c.finish(done)
	}
	return done, nil
}

// place copies a band's picture into base, making base anew when the band's size, layout or
// memory differs from what it holds.
func (c *composer) place(p *picture, unit *AccessUnit, height int) error {
	bands := min(unit.Bands, maxBands)
	if height <= 0 {
		height = bands * p.height
	}
	height = min(height, bands*p.height)
	b := c.base
	if b == nil || c.bandHeight != p.height || b.width != p.width || b.height != height || b.layout != p.layout || (b.surface == nil) != (p.surface == nil) {
		c.close()
		var err error
		if b, err = blankLike(p, p.width, height); err != nil {
			return err
		}
		c.base, c.bandHeight = b, p.height
	}
	b.fullRange, b.matrix, b.primaries, b.transfer, b.time = p.fullRange, p.matrix, p.primaries, p.transfer, p.time
	y := unit.Band * p.height
	if b.surface != nil {
		if err := b.surface.place(p.surface, y); err != nil {
			return err
		}
	} else {
		placeRows(b, p, y)
	}
	c.written |= 1 << unit.Band
	return nil
}

// finish ends the frame being composed and appends a copy of base to done, once every band
// was written into it.
func (c *composer) finish(done []composed) []composed {
	if !c.open {
		return done
	}
	c.open = false
	b := c.base
	if b == nil || c.written != 1<<min(c.last.Bands, maxBands)-1 {
		return done
	}
	copied, err := clonePicture(b)
	if err != nil {
		return done
	}
	// The frame arrived with its first band and stands for all the units up to its last.
	unit := *c.last
	unit.Arrival, unit.Received = c.first.Arrival, c.first.Received
	return append(done, composed{picture: copied, unit: &unit, bandHeight: c.bandHeight})
}

// blankLike makes a picture of the size in p's layout and memory.
func blankLike(p *picture, width, height int) (*picture, error) {
	if p.surface != nil {
		made, err := p.surface.sibling(width, height)
		if err != nil {
			return nil, err
		}
		return &picture{layout: p.layout, width: width, height: height, stride: width * 4, surface: made}, nil
	}
	return newPicture(p.layout, width, height, p.stride, height), nil
}

// clonePicture copies a picture with its colors and time.
func clonePicture(p *picture) (*picture, error) {
	if p.surface != nil {
		made, err := p.surface.clone()
		if err != nil {
			return nil, err
		}
		copied := *p
		copied.surface, copied.buffer = made, nil
		return &copied, nil
	}
	copied := newPicture(p.layout, p.width, p.height, p.stride, p.height)
	copied.fullRange, copied.matrix, copied.primaries, copied.transfer, copied.time = p.fullRange, p.matrix, p.primaries, p.transfer, p.time
	copy(copied.luma, p.luma)
	copy(copied.chroma, p.chroma)
	return copied, nil
}

// placeRows copies the rows of band into b from row y on, as many as fit; an NV12 band starts
// on an even row.
func placeRows(b, band *picture, y int) {
	rows := min(band.height, b.height-y)
	if rows <= 0 || y < 0 {
		return
	}
	rowBytes := min(b.width, band.width)
	if b.layout == layoutAYUV {
		rowBytes *= 4
	}
	for row := range rows {
		copy(b.luma[(y+row)*b.stride:(y+row)*b.stride+rowBytes], band.luma[row*band.stride:])
	}
	if b.layout == layoutNV12 {
		for row := range (rows + 1) / 2 {
			at := (y/2 + row) * b.stride
			if at+rowBytes <= len(b.chroma) && row*band.stride+rowBytes <= len(band.chroma) {
				copy(b.chroma[at:at+rowBytes], band.chroma[row*band.stride:])
			}
		}
	}
}
