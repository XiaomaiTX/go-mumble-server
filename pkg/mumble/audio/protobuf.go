package audio

import (
	"encoding/binary"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/wire"
	"math"
)

// consumeField 严格检查溢出、字段号和 group 配对；递归深度受包长限制。
func consumeField(b []byte, depth int) (field uint64, typ byte, value uint64, payload []byte, used int, err error) {
	bad := func() (uint64, byte, uint64, []byte, int, error) { return 0, 0, 0, nil, 0, ErrInvalidPacket }
	tag, n := binary.Uvarint(b)
	if n <= 0 || tag>>3 == 0 || tag>>3 > (1<<29)-1 || depth > MaxPacketSize {
		return bad()
	}
	field, typ, used = tag>>3, byte(tag&7), n
	rest := b[n:]
	switch typ {
	case 0:
		value, n = binary.Uvarint(rest)
		if n <= 0 {
			return bad()
		}
		used += n
	case 1, 5:
		n = 4
		if typ == 1 {
			n = 8
		}
		if len(rest) < n {
			return bad()
		}
		payload = rest[:n]
		used += n
	case 2:
		size, k := binary.Uvarint(rest)
		if k <= 0 || size > uint64(len(rest)-k) {
			return bad()
		}
		payload = rest[k : k+int(size)]
		used += k + int(size)
	case 3:
		for {
			end, k := binary.Uvarint(b[used:])
			if k <= 0 {
				return bad()
			}
			if end&7 == 4 {
				if end>>3 != field {
					return bad()
				}
				used += k
				break
			}
			_, _, _, _, k, e := consumeField(b[used:], depth+1)
			if e != nil {
				return bad()
			}
			used += k
		}
	default:
		return bad()
	}
	return
}

func decodeProtobuf(b []byte) (Delivery, error) {
	d := Delivery{Frame: Frame{Codec: CodecOpus}}
	if len(b) == 0 || len(b) > MaxPacketSize || b[0] != ProtobufAudioType {
		return d, ErrInvalidPacket
	}
	positions := 0
	for b = b[1:]; len(b) > 0; {
		field, typ, v, p, n, err := consumeField(b, 0)
		if err != nil {
			return d, err
		}
		b = b[n:]
		switch field {
		case 1, 2, 3, 4, 16:
			if typ != 0 {
				return d, ErrInvalidPacket
			}
			if field <= 3 && v > math.MaxUint32 {
				return d, ErrInvalidPacket
			}
			switch field {
			case 1:
				d.Target = uint32(v)
				d.Context = ContextNormal
			case 2:
				d.Target = 0
				d.Context = Context(v)
			case 3:
				d.SenderSession = uint32(v)
			case 4:
				d.FrameNumber = v
			case 16:
				d.IsTerminator = v != 0
			}
		case 5:
			if typ != 2 {
				return d, ErrInvalidPacket
			}
			d.OpusData = append([]byte(nil), p...)
		case 6:
			if (typ != 2 && typ != 5) || len(p)%4 != 0 {
				return d, ErrInvalidPacket
			}
			for len(p) > 0 {
				if positions >= 3 {
					return d, ErrInvalidPacket
				}
				d.Position[positions] = math.Float32frombits(binary.LittleEndian.Uint32(p))
				positions++
				p = p[4:]
			}
		case 7:
			if typ != 5 {
				return d, ErrInvalidPacket
			}
			d.VolumeAdjustment = math.Float32frombits(binary.LittleEndian.Uint32(p))
		}
	}
	if len(d.OpusData) == 0 || (positions != 0 && positions != 3) {
		return d, ErrInvalidPacket
	}
	d.HasPosition = positions == 3
	return d, nil
}

func encodeProtobuf(d Delivery, server bool) []byte {
	b := []byte{ProtobufAudioType}
	add := func(field int, v uint64) { b = wire.AppendTag(b, field, 0); b = wire.AppendVarint(b, v) }
	if server {
		add(2, uint64(d.Context))
		add(3, uint64(d.SenderSession))
	} else {
		add(1, uint64(d.Target))
	}
	if d.FrameNumber != 0 {
		add(4, d.FrameNumber)
	}
	b = wire.AppendBytes(b, 5, d.OpusData)
	if d.HasPosition {
		p := make([]byte, 0, 12)
		for _, v := range d.Position {
			p = wire.AppendFixed32(p, math.Float32bits(v))
		}
		b = wire.AppendBytes(b, 6, p)
	}
	if server && d.VolumeAdjustment != 0 && d.VolumeAdjustment != 1 {
		b = wire.AppendTag(b, 7, 5)
		b = wire.AppendFixed32(b, math.Float32bits(d.VolumeAdjustment))
	}
	if d.IsTerminator {
		add(16, 1)
	}
	return b
}
