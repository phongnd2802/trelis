package chunker

import (
	"unicode"
	"unicode/utf8"
)

// Language labels for token estimates.
const (
	LangEnglish    = "en"
	LangVietnamese = "vi"
	LangChinese    = "zh"
	LangMixed      = "mixed"
)

// charsPerToken is roughly how many runes one token covers in common BPE
// vocabularies, rounded down so estimates run high. Vietnamese is low
// because its tone marks break words into several tokens.
var charsPerToken = map[string]float64{
	LangEnglish:    4.0,
	LangVietnamese: 2.0,
	LangChinese:    1.7,
	LangMixed:      3.0,
}

func ratioFor(lang string) float64 {
	if r, ok := charsPerToken[lang]; ok {
		return r
	}
	return charsPerToken[LangMixed]
}

// ApproxTokenCount estimates the tokens in s for lang, without a tokenizer.
// An unknown lang counts as mixed. Non-empty text is at least one token.
func ApproxTokenCount(s, lang string) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return max(1, int(float64(n)/ratioFor(lang)+0.5))
}

// CharsForTokenLimit is the rune budget that stays within tokens for lang,
// with a 10% margin because the ratios are estimates.
func CharsForTokenLimit(tokens int, lang string) int {
	if tokens <= 0 {
		return 0
	}
	return int(float64(tokens) * ratioFor(lang) * 0.9)
}

// DetectLanguage labels text by script, cheaply, for token estimates only:
// LangChinese when CJK dominates, LangVietnamese when Latin text carries
// Vietnamese letters, LangEnglish for other Latin text, and LangMixed when
// scripts are mixed or there are no letters.
func DetectLanguage(text string) string {
	var cjk, latin, vietnamese int
	for _, r := range text {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			cjk++
		case isVietnameseLetter(r):
			vietnamese++
			latin++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	total := cjk + latin
	switch {
	case total == 0:
		return LangMixed
	case cjk*100 >= total*15 && latin*100 >= total*15:
		return LangMixed
	case cjk > latin:
		return LangChinese
	case vietnamese*100 >= latin*3:
		// Even short Vietnamese text has a letter with a diacritic in
		// most words; 3% of letters rules out loanwords in English text.
		return LangVietnamese
	default:
		return LangEnglish
	}
}

// isVietnameseLetter reports whether r is a letter only Vietnamese uses:
// đ, ă, ơ, ư, or a vowel from the Latin Extended Additional block (ạ, ế,
// ữ, ...), where most Vietnamese tone-marked vowels live. Accented letters
// French or Spanish share, such as é or á, do not count.
func isVietnameseLetter(r rune) bool {
	switch unicode.ToLower(r) {
	case 'đ', 'ă', 'ơ', 'ư':
		return true
	}
	return r >= 0x1EA0 && r <= 0x1EF9
}
