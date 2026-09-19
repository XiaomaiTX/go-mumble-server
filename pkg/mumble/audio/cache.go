package audio

import "math"

// EncodingCache 仅供一次路由调用使用：同一 Frame 的兼容接收者共享明文编码。
type EncodingCache struct{ packets map[encodingKey][]byte }
type encodingKey struct {
	mode     WireMode
	context  Context
	volume   uint32
	position bool
}

func NewEncodingCache() *EncodingCache { return &EncodingCache{packets: make(map[encodingKey][]byte)} }
func (c *EncodingCache) Encode(mode WireMode, d Delivery) ([]byte, error) {
	if c == nil {
		return EncodeServerPacket(mode, d)
	}
	key := encodingKey{mode, d.Context, math.Float32bits(d.VolumeAdjustment), d.HasPosition}
	if b, ok := c.packets[key]; ok {
		return b, nil
	}
	b, err := EncodeServerPacket(mode, d)
	if err == nil {
		c.packets[key] = b
	}
	return b, err
}

// MergeDelivery 去重时独立合并上下文和音量，不受遍历顺序影响。
func MergeDelivery(a, b Delivery) Delivery {
	if b.Context < a.Context {
		a.Context = b.Context
	}
	if b.VolumeAdjustment > a.VolumeAdjustment {
		a.VolumeAdjustment = b.VolumeAdjustment
	}
	return a
}

func (c *EncodingCache) Groups() int { return len(c.packets) }
