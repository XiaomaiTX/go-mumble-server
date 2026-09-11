package messages

import "testing"

func TestVoiceTargetChannelPresence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		target  VoiceTargetTarget
		present bool
	}{
		{"仅私聊", VoiceTargetTarget{Session: []uint32{17}}, false},
		{"根频道", VoiceTargetTarget{HasChannelID: true}, true},
		{"非根频道", VoiceTargetTarget{ChannelID: 3}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := (&VoiceTarget{ID: 1, Targets: []VoiceTargetTarget{tt.target}}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var got VoiceTarget
			if err := got.Unmarshal(data); err != nil {
				t.Fatal(err)
			}
			if len(got.Targets) != 1 || got.Targets[0].HasChannelID != tt.present {
				t.Fatalf("频道存在性错误: %+v", got)
			}
		})
	}
}

func TestVoiceTargetRejectsMalformedNestedTarget(t *testing.T) {
	var got VoiceTarget
	if err := got.Unmarshal([]byte{0x08, 1, 0x12, 1, 0x08}); err == nil {
		t.Fatal("必须返回嵌套目标解析错误")
	}
}
