package translate

import (
	"strings"
	"testing"

	"opgo/internal/config"
)

// TestEffectiveModality 验证模态解析：空默认 text->text，拆分 input/output。
func TestEffectiveModality(t *testing.T) {
	cases := []struct {
		in     string
		raw    string
		input  string
		output string
	}{
		{"", "text->text", "text", "text"},
		{"text->text", "text->text", "text", "text"},
		{"text+image+audio+video->text", "text+image+audio+video->text", "text+image+audio+video", "text"},
		{"text+image->text", "text+image->text", "text+image", "text"},
	}
	for _, tc := range cases {
		p := config.ModelPricing{Modality: tc.in}
		m := p.EffectiveModality()
		if m.Raw != tc.raw {
			t.Errorf("modality(%q).Raw = %q, want %q", tc.in, m.Raw, tc.raw)
		}
		if strings.Join(m.Input, "+") != tc.input {
			t.Errorf("modality(%q).Input = %v, want %v", tc.in, m.Input, strings.Split(tc.input, "+"))
		}
		if strings.Join(m.Output, "+") != tc.output {
			t.Errorf("modality(%q).Output = %v, want %v", tc.in, m.Output, strings.Split(tc.output, "+"))
		}
	}
}
