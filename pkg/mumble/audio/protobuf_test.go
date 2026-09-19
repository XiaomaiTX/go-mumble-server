package audio

import (
	"bytes"
	"encoding/hex"
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// 固定字节由 v1.5.915 定义手工推导，不依赖被测编码器生成。
func TestOfficialGolden(t *testing.T) {
	client := unhex(t, "0008012096012a02aabb320c0000803f000000c00000003f800101")
	server := unhex(t, "001003182a2096012a02aabb320c0000803f000000c00000003f3d00002040800101")
	f, e := DecodeClientPacket(WireProtobuf, client)
	if e != nil {
		t.Fatal(e)
	}
	if f.Target != 1 || f.FrameNumber != 150 || !f.IsTerminator || !f.HasPosition || f.Position != [3]float32{1, -2, .5} || !bytes.Equal(f.OpusData, []byte{0xaa, 0xbb}) {
		t.Fatalf("字段错误: %+v", f)
	}
	f.SenderSession = 42
	b, e := EncodeServerPacket(WireProtobuf, Delivery{Frame: f, Context: ContextListen, VolumeAdjustment: 2.5})
	if e != nil || !bytes.Equal(b, server) {
		t.Fatalf("golden: %x %v", b, e)
	}
}
func TestOfficialSchema(t *testing.T) {
	b, e := os.ReadFile("testdata/MumbleUDP.proto")
	if e != nil {
		t.Fatal(e)
	}
	for name, field := range map[string]int{"target": 1, "context": 2, "sender_session": 3, "frame_number": 4, "opus_data": 5, "positional_data": 6, "volume_adjustment": 7, "is_terminator": 16} {
		if !regexp.MustCompile(`\b` + name + `\s*=\s*` + strconv.Itoa(field) + `;`).Match(b) {
			t.Fatalf("官方字段漂移: %s", name)
		}
	}
	if ProtobufAudioType != 0 || MaxPacketSize != 1024 {
		t.Fatal("封包常量漂移")
	}
}
func TestProtobufOneofUnknownAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		body   string
		target uint32
	}{
		{"080510022a01ab", 0}, {"100208052a01ab", 5},
		{"080508072a01ab", 7},
		{"2a01ab980601a1060102030405060708aa0602ff00b50601020304", 0},
		{"2a01abbb060801c3061002c406bc06", 0},
	} {
		b := append([]byte{0}, unhex(t, tc.body)...)
		f, e := DecodeClientPacket(WireProtobuf, b)
		if e != nil || f.Target != tc.target {
			t.Fatalf("%s: %+v %v", tc.body, f, e)
		}
		for i := range b {
			b[i] = 0
		}
		if !bytes.Equal(f.OpusData, []byte{0xab}) {
			t.Fatal("引用输入缓冲区")
		}
	}
	// 客户端伪造的身份与服务端交付属性不进入 Frame。
	f, e := DecodeClientPacket(WireProtobuf, unhex(t, "0018ffffffff0f3d000020402a01ab"))
	if e != nil || f.SenderSession != 0 {
		t.Fatal(f, e)
	}
}
func TestProtobufPositionAndBoundaries(t *testing.T) {
	bits := [3]uint32{0x80000000, 0x7fc01234, 0xff800000}
	d := Delivery{Frame: Frame{Codec: CodecOpus, SenderSession: math.MaxUint32, FrameNumber: math.MaxUint64, OpusData: []byte{1}, HasPosition: true}, Context: ContextWhisper}
	for i, b := range bits {
		d.Position[i] = math.Float32frombits(b)
	}
	packed := encodeProtobuf(d, true)
	decoded, e := decodeProtobuf(packed)
	if e != nil {
		t.Fatal(e)
	}
	if decoded.SenderSession != math.MaxUint32 || decoded.FrameNumber != math.MaxUint64 {
		t.Fatal("整数边界丢失")
	}
	for i, b := range bits {
		if math.Float32bits(decoded.Position[i]) != b {
			t.Fatal("浮点位丢失")
		}
	}
	f, e := DecodeClientPacket(WireProtobuf, unhex(t, "002a0101350000803f35000000c0350000003f"))
	if e != nil || f.Position != [3]float32{1, -2, .5} {
		t.Fatal(f, e)
	}
	for _, size := range []int{1, 127, 128, 990} {
		d.OpusData = bytes.Repeat([]byte{1}, size)
		d.HasPosition = false
		b, e := EncodeServerPacket(WireProtobuf, d)
		if e != nil {
			t.Fatal(e)
		}
		got, e := decodeProtobuf(b)
		if e != nil || !bytes.Equal(got.OpusData, d.OpusData) {
			t.Fatal(e)
		}
	}
	d.OpusData = make([]byte, 1024)
	if _, e := EncodeServerPacket(WireProtobuf, d); e == nil {
		t.Fatal("未拒绝超长输出")
	}
}
func TestMalformedProtobuf(t *testing.T) {
	cases := []string{"", "01", "00", "0000", "000e", "002a00", "002a02ab", "002affffffffffffffffff01", "002a01ab200080", "002a01ab2080808080808080808002", "002a01ab0d00000000", "002a01ab320400000000", "002a01ab320100", "002a01ab3400", "002a01abba06bc07", "002a01ab3d00", "002a01ab8001", "002a01ab080000"}
	for _, s := range cases {
		if _, e := DecodeClientPacket(WireProtobuf, unhex(t, s)); e == nil {
			t.Errorf("接受坏包 %s", s)
		}
	}
	if _, e := DecodeClientPacket(WireProtobuf, make([]byte, 1025)); e == nil {
		t.Fatal("接受超长包")
	}
	valid := unhex(t, "0008012096012a02aabb320c0000803f000000c00000003f800101")
	// 只截断在字段内部；完整字段边界允许形成合法的较短消息。
	for _, i := range []int{2, 4, 5, 7, 8, 11, 12, 21, 25, 26} {
		if _, e := DecodeClientPacket(WireProtobuf, valid[:i]); e == nil {
			t.Errorf("接受截断位置 %d", i)
		}
	}
}
func TestCrossWireRoundTrip(t *testing.T) {
	f := Frame{Codec: CodecOpus, Target: 7, FrameNumber: math.MaxUint64, OpusData: []byte{0xab, 0xcd}, Position: [3]float32{1, 2, 3}, HasPosition: true, IsTerminator: true}
	for _, in := range []WireMode{WireLegacy, WireProtobuf} {
		var input []byte
		if in == WireLegacy {
			input, _ = encodeLegacy(Delivery{Frame: f}, false)
		} else {
			input = encodeProtobuf(Delivery{Frame: f}, false)
		}
		got, e := DecodeClientPacket(in, input)
		if e != nil {
			t.Fatal(e)
		}
		for _, out := range []WireMode{WireLegacy, WireProtobuf} {
			got.SenderSession = 42
			d := Delivery{Frame: got, Context: ContextWhisper}
			b, e := EncodeServerPacket(out, d)
			if e != nil {
				t.Fatal(e)
			}
			var decoded Frame
			if out == WireLegacy {
				decoded, e = decodeLegacy(b, true)
				if b[0]&31 != byte(ContextWhisper) {
					t.Fatal("Legacy 耳语上下文错误")
				}
			} else {
				dd, err := decodeProtobuf(b)
				decoded, e = dd.Frame, err
				if dd.Context != ContextWhisper {
					t.Fatal("现代上下文错误")
				}
			}
			if e != nil || decoded.FrameNumber != f.FrameNumber || decoded.SenderSession != 42 || decoded.Position != f.Position || !decoded.IsTerminator || !bytes.Equal(decoded.OpusData, f.OpusData) {
				t.Fatalf("%d -> %d: %+v %v", in, out, decoded, e)
			}
		}
	}
}
func TestStrictLegacy(t *testing.T) {
	for _, b := range [][]byte{{0x80, 1, 1, 0xab, 0}, {0x80, 0xf8, 1, 1, 1}, {0x80, 1, 0}, {0x00, 1, 1, 1}, {0x80, 1, 0xc0, 0x40, 0x00}} {
		if _, e := DecodeClientPacket(WireLegacy, b); e == nil {
			t.Fatalf("接受非法 Legacy %x", b)
		}
	}
	f, e := DecodeClientPacket(WireLegacy, []byte{0x80, 1, 1, 0xab})
	if e != nil || f.FrameNumber != 1 {
		t.Fatal(f, e)
	}
}
func FuzzDecodeProtobuf(f *testing.F) {
	f.Add([]byte{0, 0x2a, 1, 0xab})
	f.Add([]byte{0, 0x2a, 1, 0xab, 0x80, 1, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		frame, e := DecodeClientPacket(WireProtobuf, b)
		if e == nil {
			if len(frame.OpusData) == 0 || frame.SenderSession != 0 {
				t.Fatal("不变量破坏")
			}
			_, _ = EncodeServerPacket(WireProtobuf, Delivery{Frame: frame})
		}
	})
}
func FuzzDecodeLegacy(f *testing.F) {
	f.Add([]byte{0x80, 1, 1, 0xab})
	f.Fuzz(func(t *testing.T, b []byte) {
		frame, e := DecodeClientPacket(WireLegacy, b)
		if e == nil {
			_, _ = EncodeServerPacket(WireLegacy, Delivery{Frame: frame})
		}
	})
}
func BenchmarkProtobufEncode(b *testing.B) {
	d := Delivery{Frame: Frame{Codec: CodecOpus, OpusData: make([]byte, 80), SenderSession: 42, FrameNumber: 123}}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = EncodeServerPacket(WireProtobuf, d)
	}
}
func BenchmarkGroupedEncode(b *testing.B) {
	d := Delivery{Frame: Frame{Codec: CodecOpus, OpusData: make([]byte, 80), SenderSession: 42, FrameNumber: 123}}
	b.ReportAllocs()
	for b.Loop() {
		cache := NewEncodingCache()
		for i := 0; i < 100; i++ {
			_, _ = cache.Encode(WireProtobuf, d)
		}
	}
}
