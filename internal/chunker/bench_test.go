package chunker

import (
	"strings"
	"testing"
)

func BenchmarkSplit(b *testing.B) {
	inputs := map[string]string{
		"prose":             prose(4000, 8),
		"one long word":     strings.Repeat("x", 1<<20),
		"one long line":     strings.Repeat("từ ", 1<<18),
		"many tables":       strings.Repeat("Đoạn văn.\n\n"+table+"\n\n", 2000),
		"huge code block":   "```\n" + strings.Repeat("x := 1\n", 1<<17) + "```",
		"links and inlines": strings.Repeat("[liên kết](https://example.com) và `mã nguồn`\n", 16000),
	}
	for name, text := range inputs {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				if len(Split(text, DefaultConfig())) == 0 {
					b.Fatal("no chunks")
				}
			}
		})
	}
}
