package audio

import (
	"encoding/binary"
	"math"
)

func decodeLegacy(b []byte, server bool) (Frame, error) {
	var f Frame
	if len(b) < 1 {
		return f, ErrPacketTooShort
	}
	if len(b) > MaxPacketSize {
		return f, ErrInvalidPacket
	}
	f.Codec = Codec(b[0] >> 5)
	f.Target = uint32(b[0] & 31)
	b = b[1:]
	if f.Codec != CodecOpus {
		return f, ErrInvalidPacket
	}
	read := func() (uint64, error) {
		// 序号和长度只接受非负整数；0xf4 保留完整 uint64 位模式。
		if len(b) == 0 {
			return 0, ErrPacketTooShort
		}
		if b[0] >= 0xf8 {
			return 0, ErrInvalidPacket
		}
		v, n := DecodeVarint(b)
		if n <= 0 {
			return 0, ErrPacketTooShort
		}
		b = b[n:]
		return uint64(v), nil
	}
	if server {
		v, e := read()
		if e != nil {
			return f, e
		}
		if v > math.MaxUint32 {
			return f, ErrInvalidPacket
		}
		f.SenderSession = uint32(v)
	}
	seq, err := read()
	if err != nil {
		return f, err
	}
	f.FrameNumber = seq
	size, err := read()
	if err != nil {
		return f, err
	}
	if size > 0x3fff {
		return f, ErrInvalidPacket
	}
	f.IsTerminator = size&0x2000 != 0
	size &= 0x1fff
	if size > uint64(len(b)) {
		return f, ErrPacketTooShort
	}
	f.OpusData = append([]byte(nil), b[:int(size)]...)
	b = b[int(size):]
	if len(b) != 0 && len(b) != 12 {
		return f, ErrInvalidPacket
	}
	f.HasPosition = len(b) == 12
	if f.HasPosition {
		for i := range f.Position {
			f.Position[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
		}
	}
	return f, nil
}
func encodeLegacy(d Delivery, server bool) ([]byte, error) {
	if len(d.OpusData) > 0x1fff || d.Target > 31 {
		return nil, ErrInvalidPacket
	}
	header := byte(d.Target)
	if server {
		header = byte(d.Context)
	}

	b := []byte{byte(d.Codec)<<5 | header}
	add := func(v uint64) {
		var tmp [MaxVarintLen]byte
		if v > math.MaxInt64 {
			tmp[0] = 0xf4
			binary.BigEndian.PutUint64(tmp[1:], v)
			b = append(b, tmp[:9]...)
			return
		}
		n := EncodeVarint(tmp[:], int64(v))
		b = append(b, tmp[:n]...)
	}
	if server {
		add(uint64(d.SenderSession))
	}
	add(d.FrameNumber)
	size := uint64(len(d.OpusData))
	if d.IsTerminator {
		size |= 0x2000
	}
	add(size)
	b = append(b, d.OpusData...)
	if d.HasPosition {
		for _, v := range d.Position {
			b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v))
		}
	}
	return b, nil
}
