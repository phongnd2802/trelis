package chunker_test

import (
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/chunker"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{"", 0},
		{"   \t", 0},
		{"a", 1},
		{"hello", 2},    // 5 ASCII letters, 4 per token rounded up
		{"tax rate", 2}, // a word each
		{"thuế", 2},     // "thu" + "ế"
		{"Đến 5", 4},    // "Đ" + "ế" + "n" + "5"
		{"5%", 2},       // "5" + "%"
		{"a\n\n\nb", 3}, // a run of line breaks is one token
		{"税率", 2},       // a CJK rune each
		{"internationalization", 5},
	}
	for _, tt := range tests {
		if got := chunker.EstimateTokens(tt.text); got != tt.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
		}
	}
}

func BenchmarkChunk(b *testing.B) {
	inputs := map[string]string{
		"prose":           prose(4000, 8),
		"one long word":   strings.Repeat("x", 1<<20),
		"one long line":   strings.Repeat("từ ", 1<<18),
		"many tables":     strings.Repeat("Đoạn văn.\n\n"+table+"\n\n", 2000),
		"huge code block": "```\n" + strings.Repeat("x := 1\n", 1<<17) + "```",
	}
	for name, text := range inputs {
		b.Run(name, func(b *testing.B) {
			c, err := chunker.New(chunker.Config{TokenLimit: 512})
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				c.Chunk(text)
			}
		})
	}
}
