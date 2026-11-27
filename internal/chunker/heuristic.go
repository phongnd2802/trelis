package chunker

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// maxHeuristicTitleRunes is the longest line the heuristic tier accepts as a
// title. A longer line is prose that merely starts like one.
const maxHeuristicTitleRunes = 100

// Legal marker levels, shallowest first: a Chương nests under a Phần, an
// article under everything above it.
const (
	legalPartLevel    = 1
	legalChapterLevel = 2
	legalSectionLevel = 3
	legalArticleLevel = 4
)

// The legal patterns match the Vietnamese statute structure at the start of
// a line. heuristicSpace is every space a parser may emit where the text has
// a word break: tabs and \p{Zs}, which covers the non-breaking space a
// .docx/.pdf often uses. legalTitleTail takes a title on the marker's own
// line only after a space, ".", ":" or a dash, so "Mục 2abc" is not a marker;
// an empty title leaves the marker bare, so a title on the following line can
// be joined. \d is ASCII digits; the roman numeral is upper-case only, so a
// lowercase word's first letter cannot stand in for one, while (?i:...) keeps
// the marker word and "thứ" case-insensitive, so "PHẦN THỨ HAI" matches.
const (
	heuristicSpace = `[\t\p{Zs}]`
	legalTitleTail = `(?:` + heuristicSpace + `*[.:\-–—]` + heuristicSpace + `*(.*)|` + heuristicSpace + `+(.*))?$`
)

var (
	legalPart    = regexp.MustCompile(`^(?i:phần)` + heuristicSpace + `+(?:(?i:thứ)` + heuristicSpace + `+\p{L}+|\d+|[IVXLCDM]+)` + legalTitleTail)
	legalChapter = regexp.MustCompile(`^(?i:chương)` + heuristicSpace + `+(?:\d+|[IVXLCDM]+)` + legalTitleTail)
	legalSection = regexp.MustCompile(`^(?i:mục)` + heuristicSpace + `+\d+` + legalTitleTail)
	legalArticle = regexp.MustCompile(`^(?i:điều)` + heuristicSpace + `+\d+[a-zA-Z]?[.:](?:` + heuristicSpace + `|$)`)

	// Generic numbered titles: "I. Title" (level 1), "1. Title" (level 2),
	// then a level per dotted component of "1.1. Title". A dotted component
	// after the first is at most two digits, so "12.000 người" (a thousands
	// separator) is prose, not a title.
	numberedRoman  = regexp.MustCompile(`^([IVXLCDM]+)\.` + heuristicSpace + `+(\S.*)$`)
	numberedDotted = regexp.MustCompile(`^(\d+(?:\.\d{1,2})+)\.?` + heuristicSpace + `+(\S.*)$`)
	numberedArabic = regexp.MustCompile(`^(\d+)\.` + heuristicSpace + `+(\S.*)$`)
)

// heuristicLine is one line of text as the heuristic scanner sees it.
type heuristicLine struct {
	start int    // byte offset of the line start
	raw   string // the line without its trailing CR
	text  string // raw in NFC, trimmed, with emphasis markers stripped
	// protected is true when the line starts inside a fenced code block, a
	// table, math or inline code: such a line is content, never structure.
	protected bool
}

// heuristicHeadings returns the section boundaries the heuristic tier splits
// at, in document order. Two or more Điều lines ("Điều 1.") put the Document
// in legal mode, where only Vietnamese statute markers are boundaries; any
// other Document gets generic boundaries: uppercase titles, numbered titles,
// page breaks and separator lines.
//
// An ATX heading is a boundary in both modes, as a level-1 heading with its
// parsed text: the heading tier owns ATX headings, but a Document that mixes
// one with plain legal lines still needs it as a breadcrumb root. Lines
// inside protected spans are skipped, so a "Điều 2." inside a fence is code.
func heuristicHeadings(text string) []heading {
	hs, _ := scanHeuristic(text)
	return hs
}

// scanHeuristic is heuristicHeadings plus the number of Điều lines, so
// ProfileDocument can report why the tier chose legal mode. One line scan
// feeds both the count and the boundaries.
func scanHeuristic(text string) ([]heading, int) {
	protected := protectedSpans(text)
	ls := make([]heuristicLine, 0, strings.Count(text, "\n")+1)
	lines(text, 0, len(text), func(start, end int) {
		raw := strings.TrimSuffix(text[start:end], "\r")
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		ls = append(ls, heuristicLine{
			start:     start,
			raw:       raw,
			text:      cleanHeadingText(raw),
			protected: insideSpan(protected, start+indent),
		})
	})

	articles := 0
	for _, l := range ls {
		if !l.protected && legalArticle.MatchString(l.text) {
			articles++
		}
	}
	legal := articles >= 2

	var hs []heading
	for i, l := range ls {
		if l.protected {
			continue
		}
		if _, body, ok := parseATXHeading(l.raw); ok {
			hs = append(hs, heading{start: l.start, level: 1, text: cleanHeadingText(body)})
			continue
		}
		if legal {
			level, inline, ok := legalMarker(l.text)
			if !ok {
				continue
			}
			h := heading{start: l.start, level: level, text: l.text}
			if !inline && level < legalArticleLevel {
				if title, end, ok := titleAfter(ls, i, len(text)); ok {
					h.text += " " + title
					h.end = end
				}
			}
			hs = append(hs, h)
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(l.raw, " \t"), "\f") || separatorLine(strings.TrimSpace(l.raw)) {
			hs = append(hs, heading{start: l.start, level: 1, keepPath: true})
			continue
		}
		if level, ok := numberedTitle(l.text); ok {
			hs = append(hs, heading{start: l.start, level: level, text: l.text})
			continue
		}
		if uppercaseTitle(l.text) {
			hs = append(hs, heading{start: l.start, level: 1, text: l.text})
		}
	}
	return hs, articles
}

// splitHeuristic is the heuristic tier. It splits text at the boundaries
// heuristicHeadings finds and shares the heading tier's per-section budget
// and breadcrumbs: a section's first Chunk carries its ancestors, later
// Chunks the full path. Khoản lines ("1.", "2." inside an article) are
// deliberately not boundaries: splitting every clause would fragment an
// article, and an article over budget is split inside its breadcrumb at line
// ends by the recursive machinery. cfg must have its defaults applied.
func splitHeuristic(text string, cfg SplitterConfig) []Chunk {
	return splitSections(text, cfg, sectionsFor(text, heuristicHeadings(text)))
}

// legalMarker parses line as a Vietnamese legal boundary and returns its
// level: legalPartLevel for Phần, legalChapterLevel for Chương,
// legalSectionLevel for Mục and legalArticleLevel for Điều. inline reports a
// title on the marker's line, so only a bare marker looks for its title
// below. An inline title that ends like a sentence makes the line prose, not
// a marker: "Chương I của Luật này quy định như sau." is a sentence, while
// chapter titles never end with a period.
func legalMarker(line string) (level int, inline, ok bool) {
	for _, m := range [...]struct {
		re    *regexp.Regexp
		level int
	}{
		{legalPart, legalPartLevel},
		{legalChapter, legalChapterLevel},
		{legalSection, legalSectionLevel},
	} {
		sub := m.re.FindStringSubmatch(line)
		if sub == nil {
			continue
		}
		// The tail captures a title twice: after punctuation and after
		// whitespace. At most one of them matches.
		title := sub[1]
		if title == "" {
			title = sub[2]
		}
		if title != "" && endsLikeSentence(line, ".,;") {
			return 0, false, false
		}
		return m.level, title != "", true
	}
	if legalArticle.MatchString(line) {
		return legalArticleLevel, true, true
	}
	return 0, false, false
}

// titleAfter returns the uppercase title line that belongs to the marker at
// ls[i], and the byte offset just past it where the section's body starts.
// Only the first non-blank line below the marker counts, and only when it is
// an uppercase title that is not itself a boundary. A marker with no body
// after its title then yields no section, like a body-less heading.
func titleAfter(ls []heuristicLine, i, textLen int) (string, int, bool) {
	for j := i + 1; j < len(ls); j++ {
		l := ls[j]
		if l.text == "" {
			continue
		}
		if l.protected || !uppercaseTitle(l.text) {
			return "", 0, false
		}
		if _, _, ok := legalMarker(l.text); ok {
			return "", 0, false
		}
		if _, ok := numberedTitle(l.text); ok {
			return "", 0, false
		}
		if _, _, ok := parseATXHeading(l.raw); ok {
			return "", 0, false
		}
		end := textLen
		if j+1 < len(ls) {
			end = ls[j+1].start
		}
		return l.text, end, true
	}
	return "", 0, false
}

// numberedTitle returns the level of a generic numbered title line: roman
// numerals at 1, "1." at 2, then one level per dotted component of "1.1.",
// capped at 6. A roman numeral is upper-case, and a single-letter one must be
// I, V or X: "C. Phương án ba" and "D. Trần" are initialled paragraphs, not
// headings. A line that is long, has no text after its number or ends like a
// sentence is content.
func numberedTitle(line string) (int, bool) {
	if utf8.RuneCountInString(line) > maxHeuristicTitleRunes || endsLikeSentence(line, ".,;:") {
		return 0, false
	}
	if sub := numberedRoman.FindStringSubmatch(line); sub != nil {
		if len(sub[1]) > 1 || strings.Contains("IVX", sub[1]) {
			return 1, true
		}
	}
	if sub := numberedDotted.FindStringSubmatch(line); sub != nil {
		return min(strings.Count(sub[1], ".")+2, 6), true
	}
	if numberedArabic.MatchString(line) {
		return 2, true
	}
	return 0, false
}

// uppercaseTitle reports whether line is an all-caps title: at least four
// letters, every letter upper case (diacritics included), at most
// maxHeuristicTitleRunes runes, and not ending like a sentence. Punctuation
// and digits are allowed, so "ĐIỀU KHOẢN THI HÀNH" counts. The line must
// start with a letter or digit, so a table row ("| ĐIỀU 2. ... |") is never a
// title even when it opens the protected span.
func uppercaseTitle(line string) bool {
	if utf8.RuneCountInString(line) > maxHeuristicTitleRunes || endsLikeSentence(line, ".,;") {
		return false
	}
	if first, _ := utf8.DecodeRuneInString(line); !unicode.IsLetter(first) && !unicode.IsDigit(first) {
		return false
	}
	letters := 0
	for _, r := range line {
		if !unicode.IsLetter(r) {
			continue
		}
		if !unicode.IsUpper(r) {
			return false
		}
		letters++
	}
	return letters >= 4
}

// separatorLine reports whether line holds one repeated run of at least three
// "-", "*", "_" or "=" characters, spaces allowed between them.
func separatorLine(line string) bool {
	var mark rune
	n := 0
	for _, r := range line {
		switch r {
		case ' ', '\t':
			continue
		case '-', '*', '_', '=':
			if n > 0 && r != mark {
				return false
			}
			mark, n = r, n+1
		default:
			return false
		}
	}
	return n >= 3
}

// endsLikeSentence reports whether line, without trailing spaces, ends in one
// of marks. It also treats an empty line as such, so no rule accepts one.
func endsLikeSentence(line, marks string) bool {
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(line)
	return strings.ContainsRune(marks, r)
}

// cleanHeadingText is a line as the heuristic scanner reads it: NFC for
// matching and the breadcrumb, one ASCII space per run of parser whitespace,
// surrounding space trimmed, and emphasis markers stripped. A parsed .docx
// often renders "**Điều 1.** Phạm vi", and a PDF can yield decomposed
// diacritics or non-breaking spaces; the stored heading text needs none of
// them.
func cleanHeadingText(line string) string {
	return stripEmphasis(spaceRuns.ReplaceAllString(norm.NFC.String(line), " "))
}

// spaceRuns matches the parser whitespace runs cleanHeadingText collapses to
// one ASCII space.
var spaceRuns = regexp.MustCompile(heuristicSpace + `+`)

// strongEmphasis matches the strong markers, which never belong to a word.
var strongEmphasis = strings.NewReplacer("**", "", "__", "")

// stripEmphasis removes Markdown emphasis from a heading line: every
// asterisk, and an underscore unless it sits between two word characters, so
// an identifier such as "mã_số" keeps its own.
func stripEmphasis(line string) string {
	line = strongEmphasis.Replace(strings.TrimSpace(line))
	runes := []rune(line)
	var b strings.Builder
	for i, r := range runes {
		switch {
		case r == '*':
		case r == '_' && (i == 0 || i+1 == len(runes) || !wordRune(runes[i-1]) || !wordRune(runes[i+1])):
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// wordRune reports whether r can be part of a word, the context that keeps
// an underscore from being read as an emphasis marker.
func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
