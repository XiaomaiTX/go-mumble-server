package audio

// DecodePing 严格按已协商模式解析连接内探测，不根据包内容切换模式。
func DecodePing(mode WireMode, b []byte) (timestamp uint64, extended bool, err error) {
	if len(b) < 2 || len(b) > MaxPacketSize {
		return 0, false, ErrInvalidPacket
	}
	switch mode {
	case WireLegacy:
		if b[0]>>5 != 1 || b[1] >= 0xf8 {
			return 0, false, ErrInvalidPacket
		}
		v, n := DecodeVarint(b[1:])
		if n != len(b)-1 || n <= 0 {
			return 0, false, ErrInvalidPacket
		}
		return uint64(v), false, nil
	case WireProtobuf:
		if b[0] != 1 {
			return 0, false, ErrInvalidPacket
		}
		for b = b[1:]; len(b) > 0; {
			field, typ, v, _, n, e := consumeField(b, 0)
			if e != nil {
				return 0, false, e
			}
			b = b[n:]
			if field >= 1 && field <= 6 && typ != 0 {
				return 0, false, ErrInvalidPacket
			}
			if field == 1 {
				timestamp = v
			}
			if field == 2 {
				extended = v != 0
			}
		}
		return
	default:
		return 0, false, ErrInvalidPacket
	}
}
