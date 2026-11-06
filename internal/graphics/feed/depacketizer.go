// Package feed reads the device's screen as a live HEVC stream without the Objective-C engine:
// it depacketizes the RTP packets that device.Connection.OpenScreen delivers, decodes them with
// Media Foundation on Windows, and keeps the newest frame for screen.get, screen.waitForChange,
// screen.waitForStable and screen.find (SWScreenFeed.m).
package feed

import "encoding/binary"

// HEVC NAL unit types (ITU-T H.265 table 7-1) and the RTP payload structures of RFC 7798.
const (
	nalIRAPFirst   = 16 // BLA_W_LP
	nalIRAPLast    = 23 // RSV_IRAP_VCL23
	nalVPS         = 32
	nalSPS         = 33
	nalPPS         = 34
	nalAggregation = 48
	nalFragment    = 49
)

// The 13-byte record header of a delivered batch: the arrival in Unix nanoseconds, an RTCP
// flag and the packet's length, all big-endian (device.appendFeedRecord, SWScreenFeed.m).
const recordHeaderSize = 13

// NALType answers the type of an HEVC NAL unit (SWAccessUnit.m SWNALType).
func NALType(nal []byte) int {
	return int(nal[0]>>1) & 0x3f
}

// AccessUnit is the NAL units of one picture, gathered from the RTP packets that share a
// timestamp (SWAccessUnit.m). The NAL units may share memory with the batch they came from.
type AccessUnit struct {
	NALs         [][]byte
	RTPTimestamp uint32
	// Arrival is when the first packet of the unit arrived, in milliseconds since the epoch.
	// The feed takes it from its precise clock as the packet reaches it, Received.
	Arrival float64
	// Received is the reading of the feed's clock (internal/graphics/clock.Ticks) when the
	// batch with the unit's first packet reached it, 0 for units the feed did not stamp.
	Received int64
	// Profile is the "defined by profile" field of the first packet's RTP header extension.
	Profile uint16
	// Damaged is set when a packet of the unit, or one before it, was lost or malformed.
	Damaged bool
}

// IsKeyframe tells whether the unit holds an IRAP picture (types 16 to 23).
func (u *AccessUnit) IsKeyframe() bool {
	for _, nal := range u.NALs {
		if kind := NALType(nal); kind >= nalIRAPFirst && kind <= nalIRAPLast {
			return true
		}
	}
	return false
}

// HasParameterSets tells whether the unit carries a sequence parameter set.
func (u *AccessUnit) HasParameterSets() bool {
	for _, nal := range u.NALs {
		if NALType(nal) == nalSPS {
			return true
		}
	}
	return false
}

// Turns is how many quarter turns clockwise make the picture upright: the device's header
// extension 0x9xxx carries the orientation in bits 8 and 9.
func (u *AccessUnit) Turns() int {
	if u.Profile&0xf000 != 0x9000 {
		return 0
	}
	return [4]int{0, 2, 3, 1}[(u.Profile>>8)&3]
}

// Depacketizer turns RTP packets of an HEVC stream into access units (SWDepacketizer.m). It
// marks a unit damaged when a sequence number is skipped, a fragment is cut short or an
// aggregation packet is malformed; the decoder then waits for a keyframe and asks for one.
type Depacketizer struct {
	current      *AccessUnit
	fragment     []byte
	hasSequence  bool
	lastSequence uint16
	// received stamps the units that the next packets start.
	received int64
}

// Reset forgets the unit in progress and the last sequence number, as when the stream reopens.
func (d *Depacketizer) Reset() {
	d.current = nil
	d.fragment = nil
	d.hasSequence = false
}

// PushBatch parses the records of a delivered batch and pushes the RTP packets, skipping the
// RTCP ones; a record that runs past the end stops the batch (SWScreenFeed.m process:).
func (d *Depacketizer) PushBatch(batch []byte, done []*AccessUnit) []*AccessUnit {
	offset := 0
	for offset+recordHeaderSize <= len(batch) {
		arrival := binary.BigEndian.Uint64(batch[offset:])
		rtcp := batch[offset+8] != 0
		length := int(binary.BigEndian.Uint32(batch[offset+9:]))
		offset += recordHeaderSize
		if length < 0 || offset+length > len(batch) {
			break
		}
		if !rtcp {
			done = d.Push(batch[offset:offset+length], float64(arrival)/1e6, done)
		}
		offset += length
	}
	return done
}

// Push adds an RTP packet that arrived at arrival (milliseconds since the epoch) and appends
// the units it completes to done. A unit completes at its marker bit, or when a packet of a
// later timestamp comes first.
func (d *Depacketizer) Push(packet []byte, arrival float64, done []*AccessUnit) []*AccessUnit {
	if len(packet) < 12 || packet[0]>>6 != 2 {
		return done
	}
	marker := packet[1]&0x80 != 0
	sequence := binary.BigEndian.Uint16(packet[2:])
	timestamp := binary.BigEndian.Uint32(packet[4:])
	offset := 12 + 4*int(packet[0]&0x0f)
	var profile uint16
	if packet[0]&0x10 != 0 {
		if len(packet) < offset+4 {
			return done
		}
		profile = binary.BigEndian.Uint16(packet[offset:])
		offset += 4 + 4*int(binary.BigEndian.Uint16(packet[offset+2:]))
	}
	end := len(packet)
	if packet[0]&0x20 != 0 {
		end -= int(packet[end-1])
	}
	if offset+2 > end {
		return done
	}
	gap := d.hasSequence && sequence != d.lastSequence+1
	d.hasSequence = true
	d.lastSequence = sequence
	if d.current != nil && d.current.RTPTimestamp != timestamp {
		if d.fragment != nil {
			d.current.Damaged = true
			d.fragment = nil
		}
		done = append(done, d.current)
		d.current = nil
	}
	unit := d.current
	if unit == nil {
		unit = &AccessUnit{RTPTimestamp: timestamp, Arrival: arrival, Received: d.received, Profile: profile}
	}
	if gap {
		unit.Damaged = true
		d.fragment = nil
	}
	payload := packet[offset:end]
	switch NALType(payload) {
	case nalAggregation:
		addAggregated(payload, unit)
	case nalFragment:
		d.addFragment(payload, unit)
	default:
		unit.NALs = append(unit.NALs, payload)
	}
	if marker {
		done = append(done, unit)
		d.current = nil
		d.fragment = nil
	} else {
		d.current = unit
	}
	return done
}

// addAggregated splits an aggregation packet: a 2-byte payload header, then NAL units each
// behind a 16-bit size (RFC 7798 4.4.2, without DONL).
func addAggregated(payload []byte, unit *AccessUnit) {
	at := 2
	for at+2 <= len(payload) {
		size := int(binary.BigEndian.Uint16(payload[at:]))
		at += 2
		if size <= 0 || at+size > len(payload) {
			unit.Damaged = true
			break
		}
		unit.NALs = append(unit.NALs, payload[at:at+size])
		at += size
	}
}

// addFragment joins fragmentation units (RFC 7798 4.4.3, without DONL): the start rebuilds the
// NAL header from the payload header and the FU type; a fragment without its start is damage.
func (d *Depacketizer) addFragment(payload []byte, unit *AccessUnit) {
	if len(payload) <= 3 {
		unit.Damaged = true
		return
	}
	header := payload[2]
	switch {
	case header&0x80 != 0:
		d.fragment = make([]byte, 2, len(payload)*4)
		d.fragment[0] = payload[0]&0x81 | (header&0x3f)<<1
		d.fragment[1] = payload[1]
		d.fragment = append(d.fragment, payload[3:]...)
	case d.fragment != nil:
		d.fragment = append(d.fragment, payload[3:]...)
	default:
		unit.Damaged = true
	}
	if header&0x40 != 0 && d.fragment != nil {
		unit.NALs = append(unit.NALs, d.fragment)
		d.fragment = nil
	}
}
