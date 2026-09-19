package audio

import "errors"

type WireMode uint8

const (
	WireLegacy WireMode = iota
	WireProtobuf
)

type Codec uint8
type Context uint8

const (
	ContextNormal Context = iota
	ContextShout
	ContextWhisper
	ContextListen
)
const MaxPacketSize = 1024
const ProtobufAudioType byte = 0

var ErrInvalidPacket = errors.New("audio: invalid packet")

type Frame struct {
	Codec         Codec
	Target        uint32
	SenderSession uint32
	FrameNumber   uint64
	OpusData      []byte
	Position      [3]float32
	HasPosition   bool
	IsTerminator  bool
}
type Delivery struct {
	Frame
	Context          Context
	VolumeAdjustment float32
}

// NegotiateWireMode 同时考虑双方能力，避免服务端仍宣告 1.4 时误读现代客户端。
func NegotiateWireMode(clientVersion, serverVersion uint64) WireMode {
	const modern = uint64(1<<48 | 5<<32)
	if clientVersion >= modern && serverVersion >= modern {
		return WireProtobuf
	}
	return WireLegacy
}

func DecodeClientPacket(mode WireMode, data []byte) (Frame, error) {
	if len(data) == 0 {
		return Frame{}, ErrPacketTooShort
	}
	if len(data) > MaxPacketSize {
		return Frame{}, ErrInvalidPacket
	}
	var f Frame
	var err error
	switch mode {
	case WireLegacy:
		f, err = decodeLegacy(data, false)
	case WireProtobuf:
		var d Delivery
		d, err = decodeProtobuf(data)
		f = d.Frame
	default:
		err = ErrInvalidPacket
	}
	// 身份只能由认证连接赋值，输入中的服务端字段不能越过此边界。
	f.SenderSession = 0
	if err == nil && (f.Target > 31 || f.Codec != CodecOpus || len(f.OpusData) == 0) {
		err = ErrInvalidPacket
	}
	return f, err
}
func EncodeServerPacket(mode WireMode, d Delivery) ([]byte, error) {
	if d.Codec != CodecOpus || len(d.OpusData) == 0 || len(d.OpusData) > MaxPacketSize || d.Context > ContextListen {
		return nil, ErrInvalidPacket
	}
	var b []byte
	var err error
	switch mode {
	case WireLegacy:
		b, err = encodeLegacy(d, true)
	case WireProtobuf:
		b = encodeProtobuf(d, true)
	default:
		err = ErrInvalidPacket
	}
	if len(b) > MaxPacketSize {
		return nil, ErrInvalidPacket
	}
	return b, err
}
