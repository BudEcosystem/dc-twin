package ipmi

// The record formats a BMC serves from its repositories: Full Sensor Records (SDR, IPMI v2.0
// section 43.1), System Event Log records (32.1) and the FRU inventory (Platform Management FRU
// Information Storage Definition v1.0). All are built from the model; none is captured.

import (
	"math"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

const (
	sdrVersion       = 0x51
	sdrFullSensor    = 0x01
	sensorTypeTemp   = 0x01
	sensorTypeOther  = 0x0B // other units-based sensor: power in watts
	unitDegreesC     = 0x01
	unitWatts        = 0x06
	readingThreshold = 0x01
	selCapacity      = 1024
	selRecordBytes   = 16
	fruFormatVersion = 0x01
)

// sensor is one Full Sensor Record bound to a profile sensor.
type sensor struct {
	num    byte
	spec   schema.SensorSpec
	m      int  // reading = m × raw (B = 0, both exponents 0)
	unc    byte // upper non-critical (the profile's warn), raw
	ucr    byte // upper critical (the profile's crit), raw
	read   byte // readable threshold mask
	record []byte
}

// sensorsFor builds the SDR repository for a profile: its temperature and power sensors, in
// profile order, numbered from 1. Readings are 8-bit and linear as on real BMCs, so a power
// sensor's resolution is the profile's maximum draw over 255 (with headroom).
func sensorsFor(p *schema.DeviceProfile) []*sensor {
	var out []*sensor
	for _, s := range p.Spec.Sensors {
		if s.Quantity != "temperature" && s.Quantity != "power" {
			continue
		}
		x := &sensor{num: byte(len(out) + 1), spec: s, m: 1}
		top := 0.0
		if s.Quantity == "power" {
			top = math.Max(p.Spec.Power.MaxW, p.Spec.Power.OutputW) * 1.2
		}
		for _, t := range []*float64{s.Warn, s.Crit} {
			if t != nil {
				top = math.Max(top, *t*1.2)
			}
		}
		if top > 255 {
			x.m = int(math.Ceil(top / 255))
		}
		if s.Warn != nil {
			x.unc, x.read = x.raw(*s.Warn), x.read|0x08
		}
		if s.Crit != nil {
			x.ucr, x.read = x.raw(*s.Crit), x.read|0x10
		}
		x.record = x.full(uint16(x.num))
		out = append(out, x)
	}
	return out
}

func (x *sensor) raw(v float64) byte {
	r := math.Round(v / float64(x.m))
	return byte(math.Max(0, math.Min(255, r)))
}

func entityFor(source string) byte {
	switch source {
	case "inlet":
		return 0x37 // air inlet
	case "cpu-max":
		return 0x03 // processor
	case "accelerator-max", "asic-max":
		return 0x0B // add-in card
	case "power", "output-power":
		return 0x13 // power unit / power domain
	}
	return 0x07 // system board
}

// full encodes the Full Sensor Record (Table 43-1).
func (x *sensor) full(id uint16) []byte {
	stype, unit := byte(sensorTypeTemp), byte(unitDegreesC)
	if x.spec.Quantity == "power" {
		stype, unit = sensorTypeOther, unitWatts
	}
	var assert, deassert uint16
	if x.read&0x08 != 0 {
		assert |= 1 << 7    // upper non-critical going high
		deassert |= 1 << 12 // upper non-critical comparison returned
	}
	if x.read&0x10 != 0 {
		assert |= 1 << 9
		deassert |= 1 << 13
	}
	deassert |= assert & 0x0FFF
	caps := byte(0x40) // auto re-arm, no hysteresis, per-threshold event control
	if x.read != 0 {
		caps |= 0x04 // thresholds readable
	}
	name := x.spec.Name
	if len(name) > 16 {
		name = name[:16]
	}
	b := []byte{0, 0, sdrVersion, sdrFullSensor, 0,
		bmcSlaveAddr, 0x00, x.num, // key: owner, LUN, number
		entityFor(x.spec.Source), 0x01, // entity id, instance
		0x7F, caps, stype, readingThreshold}
	b = append(b, put16(assert)...)
	b = append(b, put16(deassert)...)
	b = append(b, x.read, 0x00)           // readable thresholds (LS byte), settable thresholds: none
	b = append(b, 0x00, unit, 0x00, 0x00, // units 1 (unsigned), base unit, modifier, linear
		byte(x.m&0xFF), byte((x.m>>8)&0x03)<<6, // M, tolerance
		0x00, 0x00, 0x00, // B, accuracy
		0x00,             // R and B exponents
		0x00,             // analog characteristics: nothing nominal
		0x00, 0x00, 0x00, // nominal, normal max, normal min
		0xFF, 0x00, // sensor maximum, minimum
		0x00, x.ucr, x.unc, 0x00, 0x00, 0x00, // UNR, UCR, UNC, LNR, LCR, LNC
		0x00, 0x00, // hysteresis
		0x00, 0x00, 0x00, // reserved, OEM
		0xC0|byte(len(name)))
	b = append(b, name...)
	copy(b[0:2], put16(id))
	b[4] = byte(len(b) - 5)
	return b
}

// status is the threshold comparison byte of Get Sensor Reading, from the engine's committed
// threshold state (so IPMI's verdict is the model's, not a re-quantised comparison).
func thresholdBits(state string) byte {
	switch state {
	case model.Critical:
		return 0x18 // at or above UNC and UCR
	case model.Warning:
		return 0x08
	}
	return 0
}

// ---------------------------------------------------------------- SEL

// selRecord encodes a model log entry as a system event record (Table 32-1). The model's log
// carries Redfish messages; IPMI's SEL carries only an event class, so the message is mapped to
// the nearest sensor-specific offset by its registry key and its text is not carried.
func selRecord(id uint16, l model.LogEntry) []byte {
	stype, offset := byte(0x12), byte(0x02) // System Event: undetermined system hardware failure
	key := l.MessageID
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[i+1:]
	}
	switch key {
	case "PowerSupplyFailure":
		stype, offset = 0x08, 0x01 // power supply: failure detected
	case "OutputLost", "PowerSupplyInputLost":
		stype, offset = 0x08, 0x03 // power supply: input lost
	case "LeakDetected":
		stype, offset = 0x12, 0x02
	default:
		if l.Severity == model.OK || l.Severity == "" {
			stype, offset = 0x12, 0x03 // System Event: entry added to auxiliary log
		}
	}
	ts := uint32(l.Created.Unix())
	b := put16(id)
	b = append(b, 0x02) // system event record
	b = append(b, put32(ts)...)
	b = append(b, put16(bmcSlaveAddr)...) // generator: the BMC
	b = append(b, 0x04, stype, 0x00, 0x6F, offset, 0xFF, 0xFF)
	return b
}

// ---------------------------------------------------------------- FRU

// fruField is a type/length byte (8-bit ASCII + Latin-1) and the field.
func fruField(s string) []byte {
	if len(s) > 63 {
		s = s[:63]
	}
	return append([]byte{0xC0 | byte(len(s))}, s...)
}

// fruArea closes an area: end-of-fields, padding to 8 bytes, the length and the checksum.
func fruArea(head []byte, fields ...[]byte) []byte {
	b := append([]byte(nil), head...)
	for _, f := range fields {
		b = append(b, f...)
	}
	b = append(b, 0xC1)
	for (len(b)+1)%8 != 0 {
		b = append(b, 0x00)
	}
	b[1] = byte((len(b) + 1) / 8)
	return append(b, checksum(b))
}

// fruImage is FRU device 0: chassis, board and product areas with the identity's
// manufacturer, model, part number and serial.
func fruImage(spec *schema.IdentitySpec, serial string) []byte {
	if spec == nil {
		spec = &schema.IdentitySpec{}
	}
	chassis := fruArea([]byte{fruFormatVersion, 0, 0x17}, // rack mount chassis
		fruField(spec.PartNumber), fruField(serial))
	board := fruArea([]byte{fruFormatVersion, 0, 0x00, 0, 0, 0}, // English, manufacturing date unspecified
		fruField(spec.Manufacturer), fruField(spec.Model), fruField(serial), fruField(spec.PartNumber), fruField(""))
	product := fruArea([]byte{fruFormatVersion, 0, 0x00},
		fruField(spec.Manufacturer), fruField(spec.Model), fruField(spec.PartNumber), fruField(""),
		fruField(serial), fruField(""), fruField(""))
	header := []byte{fruFormatVersion, 0, 1, byte(1 + len(chassis)/8), byte(1 + (len(chassis)+len(board))/8), 0, 0}
	header = append(header, checksum(header))
	out := append(header, chassis...)
	out = append(out, board...)
	return append(out, product...)
}

// unixOr is t as an IPMI timestamp, or "unspecified".
func unixOr(t time.Time) uint32 {
	if t.IsZero() {
		return 0xFFFFFFFF
	}
	return uint32(t.Unix())
}
