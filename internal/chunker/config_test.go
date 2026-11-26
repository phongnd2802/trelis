package chunker

import (
	"slices"
	"testing"
)

func TestEnsureDefaults(t *testing.T) {
	tests := []struct {
		name        string
		in          SplitterConfig
		wantSize    int
		wantOverlap int
	}{
		{"empty config gets both defaults", SplitterConfig{}, DefaultChunkSize, DefaultChunkOverlap},
		{"explicit zero overlap is kept", SplitterConfig{ChunkSize: 300}, 300, 0},
		{"unset size with zero overlap gets the default overlap", SplitterConfig{ChunkOverlap: 0}, DefaultChunkSize, DefaultChunkOverlap},
		{"negative overlap becomes the default", SplitterConfig{ChunkSize: 300, ChunkOverlap: -1}, 300, DefaultChunkOverlap},
		{"overlap is capped at half the size", SplitterConfig{ChunkSize: 100, ChunkOverlap: 90}, 100, 50},
		{"no token limit keeps the size", SplitterConfig{ChunkSize: 800, ChunkOverlap: 10}, 800, 10},
		// 100 tokens * 4 chars/token * 0.9 safety = 360 chars.
		{"token limit clamps the size", SplitterConfig{ChunkSize: 10000, ChunkOverlap: 10, TokenLimit: 100, Languages: []string{LangEnglish}}, 360, 10},
		{"token limit above the size keeps the size", SplitterConfig{ChunkSize: 200, ChunkOverlap: 10, TokenLimit: 1000, Languages: []string{LangEnglish}}, 200, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ensureDefaults(tt.in, "")
			if got.ChunkSize != tt.wantSize || got.ChunkOverlap != tt.wantOverlap {
				t.Fatalf("size, overlap = %d, %d; want %d, %d", got.ChunkSize, got.ChunkOverlap, tt.wantSize, tt.wantOverlap)
			}
			if len(got.Separators) == 0 {
				t.Fatal("no separators after defaults")
			}
		})
	}
}

func TestTokenLimitIsTighterForVietnameseThanEnglish(t *testing.T) {
	en := ensureDefaults(SplitterConfig{TokenLimit: 200, Languages: []string{LangEnglish}}, "")
	vi := ensureDefaults(SplitterConfig{TokenLimit: 200, Languages: []string{LangVietnamese}}, "")
	if vi.ChunkSize >= en.ChunkSize {
		t.Fatalf("Vietnamese char budget %d should be below English %d", vi.ChunkSize, en.ChunkSize)
	}
}

func TestTokenLimitDetectsTheLanguageWhenNoneIsGiven(t *testing.T) {
	text := "Thuế thu nhập cá nhân là loại thuế trực thu, đánh vào thu nhập của cá nhân."
	got := ensureDefaults(SplitterConfig{TokenLimit: 200}, text)
	want := ensureDefaults(SplitterConfig{TokenLimit: 200, Languages: []string{LangVietnamese}}, "")
	if got.ChunkSize != want.ChunkSize {
		t.Fatalf("ChunkSize = %d, want the Vietnamese budget %d", got.ChunkSize, want.ChunkSize)
	}
}

func TestNormalizeSplitterConfigMatchesEnsureDefaults(t *testing.T) {
	cfg := NormalizeSplitterConfig(SplitterConfig{})
	if cfg.ChunkSize != DefaultChunkSize || cfg.ChunkOverlap != DefaultChunkOverlap {
		t.Fatalf("size, overlap = %d, %d", cfg.ChunkSize, cfg.ChunkOverlap)
	}
	if !slices.Equal(cfg.Separators, DefaultConfig().Separators) {
		t.Fatalf("separators = %q", cfg.Separators)
	}
	if kept := NormalizeSplitterConfig(SplitterConfig{ChunkSize: 256}); kept.ChunkOverlap != 0 {
		t.Fatalf("explicit zero overlap became %d", kept.ChunkOverlap)
	}
}

func TestNormalizeLineEndings(t *testing.T) {
	if got := NormalizeLineEndings("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApproxTokenCount(t *testing.T) {
	tests := []struct {
		text, lang string
		want       int
	}{
		{"", LangEnglish, 0},
		{"abcdefgh", LangEnglish, 2},         // 8 chars / 4
		{"thuế thu nhập", LangVietnamese, 7}, // 13 chars / 2, rounded
		{"税率很高", LangChinese, 2},             // 4 chars / 1.7, rounded
		{"x", LangEnglish, 1},                // never below one token
		{"abcdef", "klingon", 2},             // unknown: mixed, 6 / 3
	}
	for _, tt := range tests {
		if got := ApproxTokenCount(tt.text, tt.lang); got != tt.want {
			t.Errorf("ApproxTokenCount(%q, %q) = %d, want %d", tt.text, tt.lang, got, tt.want)
		}
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct{ text, want string }{
		{"", LangMixed},
		{"The quick brown fox jumps over the lazy dog.", LangEnglish},
		{"Điều 1. Phạm vi điều chỉnh của Luật thuế thu nhập cá nhân.", LangVietnamese},
		{"个人所得税是对个人取得的各项所得征收的一种税。", LangChinese},
		{"12345 !!!", LangMixed},
	}
	for _, tt := range tests {
		if got := DetectLanguage(tt.text); got != tt.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestCharsForTokenLimit(t *testing.T) {
	if got := CharsForTokenLimit(100, LangEnglish); got != 360 {
		t.Errorf("English: got %d, want 360 (4 chars/token with a 0.9 margin)", got)
	}
	if got := CharsForTokenLimit(0, LangEnglish); got != 0 {
		t.Errorf("zero tokens: got %d, want 0", got)
	}
}
