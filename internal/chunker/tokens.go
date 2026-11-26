package chunker

import "unicode/utf8"

// TokenCounter returns how many tokens s takes. It must not decrease when
// s grows, which every real tokenizer satisfies closely enough.
type TokenCounter func(s string) int

// EstimateTokens is the default TokenCounter. It needs no tokenizer and
// errs on the high side, so a Chunk it counts within a limit stays within
// that limit for common BPE vocabularies:
//
//   - a run of ASCII letters and digits costs one token per four bytes,
//     rounded up, since BPE merges common English subwords;
//   - every other non-space rune costs a token on its own. This covers
//     Vietnamese letters with diacritics (which split the word around
//     them), CJK, and punctuation;
//   - a run of spaces and tabs is free (BPE folds it into the next word),
//     and a run of line breaks costs one.
func EstimateTokens(s string) int {
	tokens, word := 0, 0
	flush := func() {
		tokens += (word + 3) / 4
		word = 0
	}
	newline := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r < utf8.RuneSelf && isASCIIAlnum(byte(r)):
			word++
			newline = false
			continue
		case r == ' ' || r == '\t':
			flush()
			newline = false
		case r == '\n' || r == '\r':
			flush()
			if !newline {
				tokens++
			}
			newline = true
		default:
			flush()
			tokens++
			newline = false
		}
	}
	flush()
	return tokens
}

func isASCIIAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
