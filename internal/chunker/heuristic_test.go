package chunker

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// wantHeading is one expected boundary of heuristicHeadings.
type wantHeading struct {
	start    int
	level    int
	text     string
	keepPath bool
	end      int
}

// assertHeuristicHeadings compares the heuristic scanner's output with want,
// including the byte offset where a joined heading's body starts.
func assertHeuristicHeadings(t *testing.T, text string, want []wantHeading) {
	t.Helper()
	got := heuristicHeadings(text)
	if len(got) != len(want) {
		t.Fatalf("heuristicHeadings = %+v, want %+v", got, want)
	}
	for i, w := range want {
		g := got[i]
		if g.start != w.start || g.level != w.level || g.text != w.text || g.keepPath != w.keepPath || g.end != w.end {
			t.Errorf("heading %d = {start:%d level:%d text:%q keepPath:%v end:%d}, want {start:%d level:%d text:%q keepPath:%v end:%d}",
				i, g.start, g.level, g.text, g.keepPath, g.end, w.start, w.level, w.text, w.keepPath, w.end)
		}
	}
}

// loadLegalSample reads the synthetic Vietnamese law the tier tests use.
func loadLegalSample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/luat_mau.md")
	if err != nil {
		t.Fatalf("read the sample legal document: %v", err)
	}
	return string(b)
}

// uppercaseTitledDocument is the generic-mode fixture: three all-caps
// section titles, each followed by body prose.
func uppercaseTitledDocument() string {
	return "QUY ĐỊNH CHUNG\n\n" + prose(2, 3) +
		"\n\nTHỦ TỤC HÀNH CHÍNH\n\n" + prose(2, 3) +
		"\n\nĐIỀU KHOẢN THI HÀNH\n\n" + prose(2, 3)
}

func TestHeuristicHeadings(t *testing.T) {
	markers := "Mở đầu.\n\nPhần thứ nhất\n\nChương I\n\nMục 2. Quy định chung\n\nĐiều 1. Phạm vi điều chỉnh\n\nPHẦN II\n\nĐiều 2. Đối tượng nộp thuế"
	digits := "Phần 2\n\nChương 3\n\nMục 4\n\nĐiều 10a. Phạm vi\n\nĐiều 11. Đối tượng"
	partialBold := "*Chương I*\nQUY ĐỊNH CHUNG\n\n**Mục 1.** Quy định chung\n\n**Điều 1.** Phạm vi điều chỉnh\nNội dung.\n\n__Điều 2.__ Đối tượng nộp thuế\nNội dung."
	inlineTitles := "Chương I QUY ĐỊNH CHUNG\n\nĐiều 1. Phạm vi\nNội dung.\n\nChương 2 - Quy định chung\n\nĐiều 2. Đối tượng\nNội dung.\n\nChương 3 – Quy định riêng\n\nĐiều 3. Hiệu lực\nNội dung.\n\nChương 4 — Quy định khác\n\nĐiều 4. Điều khoản\nNội dung."
	colonArticles := "Điều 1: Phạm vi điều chỉnh\nNội dung.\n\nĐiều 2: Đối tượng nộp thuế\nNội dung."
	nbsp := "\u00a0"
	nonBreakingSpace := "Chương" + nbsp + "I\nQUY ĐỊNH CHUNG\n\nĐiều" + nbsp + "1. Phạm vi\nNội dung.\n\nĐiều" + nbsp + "2. Đối tượng\nNội dung."
	decomposed := norm.NFD.String("Chương I\nQUY ĐỊNH CHUNG\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung.")
	decomposedMarker := norm.NFD.String("Chương I")
	decomposedTitle := norm.NFD.String("QUY ĐỊNH CHUNG")
	decomposedArticle1 := norm.NFD.String("Điều 1. Phạm vi")
	decomposedArticle2 := norm.NFD.String("Điều 2. Đối tượng")
	proseMarkers := "Mở đầu.\n\nPhần lớn các trường hợp được miễn.\n\nChương mới của đời sống.\n\nMục 2abc\n\nPhần thứ ba của Luật này được áp dụng.\n\nChương I của Luật này quy định như sau.\n\nChương iv\n\nChương I\nQUY ĐỊNH CHUNG\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung."
	romanLookalikes := "C. Phương án ba\nNội dung.\n\nD. Trần\nNội dung.\n\nIV. Bốn\nNội dung.\n\nX. Mười\nNội dung."
	thousands := "12.000 người tham gia\nNội dung.\n\nI. QUY ĐỊNH CHUNG\nNội dung."
	joined := "Chương I\nQUY ĐỊNH CHUNG\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung."
	numberedAfter := "Chương I\n1. PHẠM VI\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung."
	inline := "Chương I. QUY ĐỊNH CHUNG\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung."
	emphasis := "**Chương I**\n**QUY ĐỊNH CHUNG**\n\n__Điều 1. Phạm vi__\nNội dung.\n\n*Điều 2. Đối tượng*\nNội dung."
	upper := "PHẦN THỨ HAI\n\nCHƯƠNG III\n\nMỤC 4\n\nĐIỀU 7. Nội dung\n\nĐIỀU 8. Nội dung"
	legalOnly := "LUẬT MẪU\n\nChương I\nQUY ĐỊNH CHUNG\n\nĐiều 1. Phạm vi\n\nI. Một mục\n\n1. Một mục khác\n\nKhoản 1. Nội dung\n\nĐiều 2. Đối tượng\n\n---"
	lookalikes := "Mục tiêu của Luật là bảo đảm công bằng.\n\nĐiều 5 của Luật này quy định về thời hạn.\n\nĐiều 7 Phạm vi không có dấu chấm.\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung."
	numbered := "I. QUY ĐỊNH CHUNG\nNội dung.\n\n1. Người nộp thuế phải kê khai và nộp thuế đúng hạn.\nNội dung.\n\nII. PHẦN TIẾP THEO\nNội dung."
	dotted := "I. QUY ĐỊNH CHUNG\nNội dung.\n\n1. Phạm vi\nNội dung.\n\n1.1. Đối tượng\nNội dung.\n\n1.1.1 Chi tiết\nNội dung.\n\n1.1.1.1.1.1 Sáu cấp\nNội dung."
	uppercaseDoc := "QUY ĐỊNH CHUNG\nNội dung.\n\nQUYỀN VÀ NGHĨA VỤ CỦA NGƯỜI NỘP THUẾ\nNội dung.\n\nĐIỀU KHOẢN THI HÀNH\nNội dung."
	breaks := "QUY ĐỊNH CHUNG\nNội dung.\n\n\fPHẦN TIẾP THEO\nNội dung tiếp.\n\n- - -\nNội dung cuối."
	longLines := "QUY ĐỊNH CHUNG VỀ " + strings.Repeat("THUẾ ", 20) + "\n\n1. " + strings.Repeat("Nội dung ", 20) + "cuối"
	atx := "# LUẬT MẪU\n\nĐiều 1. Phạm vi\nNội dung.\n\nĐiều 2. Đối tượng\nNội dung."
	protected := "```\nĐiều 1. Trong mã\nĐiều 2. Trong mã\nQUY ĐỊNH CHUNG\n---\n```\n\n| ĐIỀU 2. PHẠM VI | x |\n|---|---|\n| ĐIỀU 3. NỘI DUNG | y |\n\nĐIỀU KHOẢN THI HÀNH\nNội dung."

	tests := []struct {
		name string
		text string
		want []wantHeading
	}{
		{
			name: "each legal marker keeps its level",
			text: markers,
			want: []wantHeading{
				{start: strings.Index(markers, "Phần thứ nhất"), level: 1, text: "Phần thứ nhất"},
				{start: strings.Index(markers, "Chương I"), level: 2, text: "Chương I"},
				{start: strings.Index(markers, "Mục 2. Quy định chung"), level: 3, text: "Mục 2. Quy định chung"},
				{start: strings.Index(markers, "Điều 1. Phạm vi điều chỉnh"), level: 4, text: "Điều 1. Phạm vi điều chỉnh"},
				{start: strings.Index(markers, "PHẦN II"), level: 1, text: "PHẦN II"},
				{start: strings.Index(markers, "Điều 2. Đối tượng nộp thuế"), level: 4, text: "Điều 2. Đối tượng nộp thuế"},
			},
		},
		{
			name: "digit markers and a letter-suffixed article",
			text: digits,
			want: []wantHeading{
				{start: 0, level: 1, text: "Phần 2"},
				{start: strings.Index(digits, "Chương 3"), level: 2, text: "Chương 3"},
				{start: strings.Index(digits, "Mục 4"), level: 3, text: "Mục 4"},
				{start: strings.Index(digits, "Điều 10a. Phạm vi"), level: 4, text: "Điều 10a. Phạm vi"},
				{start: strings.Index(digits, "Điều 11. Đối tượng"), level: 4, text: "Điều 11. Đối tượng"},
			},
		},
		{
			name: "partial bold drops every emphasis marker",
			text: partialBold,
			want: []wantHeading{
				{
					start: 0,
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(partialBold, "QUY ĐỊNH CHUNG") + len("QUY ĐỊNH CHUNG") + 1,
				},
				{start: strings.Index(partialBold, "**Mục 1.**"), level: 3, text: "Mục 1. Quy định chung"},
				{start: strings.Index(partialBold, "**Điều 1.**"), level: 4, text: "Điều 1. Phạm vi điều chỉnh"},
				{start: strings.Index(partialBold, "__Điều 2.__"), level: 4, text: "Điều 2. Đối tượng nộp thuế"},
			},
		},
		{
			name: "a space or dash separates a same-line title",
			text: inlineTitles,
			want: []wantHeading{
				{start: 0, level: 2, text: "Chương I QUY ĐỊNH CHUNG"},
				{start: strings.Index(inlineTitles, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(inlineTitles, "Chương 2 - Quy định chung"), level: 2, text: "Chương 2 - Quy định chung"},
				{start: strings.Index(inlineTitles, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
				{start: strings.Index(inlineTitles, "Chương 3 – Quy định riêng"), level: 2, text: "Chương 3 – Quy định riêng"},
				{start: strings.Index(inlineTitles, "Điều 3. Hiệu lực"), level: 4, text: "Điều 3. Hiệu lực"},
				{start: strings.Index(inlineTitles, "Chương 4 — Quy định khác"), level: 2, text: "Chương 4 — Quy định khác"},
				{start: strings.Index(inlineTitles, "Điều 4. Điều khoản"), level: 4, text: "Điều 4. Điều khoản"},
			},
		},
		{
			name: "an article marker may end with a colon",
			text: colonArticles,
			want: []wantHeading{
				{start: 0, level: 4, text: "Điều 1: Phạm vi điều chỉnh"},
				{start: strings.Index(colonArticles, "Điều 2: Đối tượng nộp thuế"), level: 4, text: "Điều 2: Đối tượng nộp thuế"},
			},
		},
		{
			name: "non-breaking spaces separate markers from numbers",
			text: nonBreakingSpace,
			want: []wantHeading{
				{
					start: 0,
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(nonBreakingSpace, "QUY ĐỊNH CHUNG") + len("QUY ĐỊNH CHUNG") + 1,
				},
				{start: strings.Index(nonBreakingSpace, "Điều"+nbsp+"1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(nonBreakingSpace, "Điều"+nbsp+"2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "decomposed diacritics match and store NFC",
			text: decomposed,
			want: []wantHeading{
				{
					start: strings.Index(decomposed, decomposedMarker),
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(decomposed, decomposedTitle) + len(decomposedTitle) + 1,
				},
				{start: strings.Index(decomposed, decomposedArticle1), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(decomposed, decomposedArticle2), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "uppercase legal markers match too",
			text: upper,
			want: []wantHeading{
				{start: 0, level: 1, text: "PHẦN THỨ HAI"},
				{start: strings.Index(upper, "CHƯƠNG III"), level: 2, text: "CHƯƠNG III"},
				{start: strings.Index(upper, "MỤC 4"), level: 3, text: "MỤC 4"},
				{start: strings.Index(upper, "ĐIỀU 7. Nội dung"), level: 4, text: "ĐIỀU 7. Nội dung"},
				{start: strings.Index(upper, "ĐIỀU 8. Nội dung"), level: 4, text: "ĐIỀU 8. Nội dung"},
			},
		},
		{
			name: "an uppercase title under a marker joins it",
			text: joined,
			want: []wantHeading{
				{
					start: 0,
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(joined, "QUY ĐỊNH CHUNG") + len("QUY ĐỊNH CHUNG") + 1,
				},
				{start: strings.Index(joined, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(joined, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "a numbered line is not joined as a title",
			text: numberedAfter,
			want: []wantHeading{
				{start: 0, level: 2, text: "Chương I"},
				{start: strings.Index(numberedAfter, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(numberedAfter, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "a title on the marker line stays as it is",
			text: inline,
			want: []wantHeading{
				{start: 0, level: 2, text: "Chương I. QUY ĐỊNH CHUNG"},
				{start: strings.Index(inline, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(inline, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "wrapping emphasis markers are stripped",
			text: emphasis,
			want: []wantHeading{
				{
					start: 0,
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(emphasis, "**QUY ĐỊNH CHUNG**") + len("**QUY ĐỊNH CHUNG**") + 1,
				},
				{start: strings.Index(emphasis, "__Điều 1. Phạm vi__"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(emphasis, "*Điều 2. Đối tượng*"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "legal mode ignores uppercase titles and numbered lines",
			text: legalOnly,
			want: []wantHeading{
				{
					start: strings.Index(legalOnly, "Chương I"),
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(legalOnly, "QUY ĐỊNH CHUNG") + len("QUY ĐỊNH CHUNG") + 1,
				},
				{start: strings.Index(legalOnly, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(legalOnly, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "marker lookalikes are content",
			text: lookalikes,
			want: []wantHeading{
				{start: strings.Index(lookalikes, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(lookalikes, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "prose lines that start like Phần or Chương are content",
			text: proseMarkers,
			want: []wantHeading{
				{
					start: strings.Index(proseMarkers, "Chương I\nQUY ĐỊNH CHUNG"),
					level: 2,
					text:  "Chương I QUY ĐỊNH CHUNG",
					end:   strings.Index(proseMarkers, "QUY ĐỊNH CHUNG") + len("QUY ĐỊNH CHUNG") + 1,
				},
				{start: strings.Index(proseMarkers, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(proseMarkers, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "a long numbered sentence is not a title",
			text: numbered,
			want: []wantHeading{
				{start: 0, level: 1, text: "I. QUY ĐỊNH CHUNG"},
				{start: strings.Index(numbered, "II. PHẦN TIẾP THEO"), level: 1, text: "II. PHẦN TIẾP THEO"},
			},
		},
		{
			name: "roman arabic and dotted numbering",
			text: dotted,
			want: []wantHeading{
				{start: 0, level: 1, text: "I. QUY ĐỊNH CHUNG"},
				{start: strings.Index(dotted, "1. Phạm vi"), level: 2, text: "1. Phạm vi"},
				{start: strings.Index(dotted, "1.1. Đối tượng"), level: 3, text: "1.1. Đối tượng"},
				{start: strings.Index(dotted, "1.1.1 Chi tiết"), level: 4, text: "1.1.1 Chi tiết"},
				{start: strings.Index(dotted, "1.1.1.1.1.1 Sáu cấp"), level: 6, text: "1.1.1.1.1.1 Sáu cấp"},
			},
		},
		{
			name: "uppercase titles with diacritics",
			text: uppercaseDoc,
			want: []wantHeading{
				{start: 0, level: 1, text: "QUY ĐỊNH CHUNG"},
				{start: strings.Index(uppercaseDoc, "QUYỀN VÀ NGHĨA VỤ CỦA NGƯỜI NỘP THUẾ"), level: 1, text: "QUYỀN VÀ NGHĨA VỤ CỦA NGƯỜI NỘP THUẾ"},
				{start: strings.Index(uppercaseDoc, "ĐIỀU KHOẢN THI HÀNH"), level: 1, text: "ĐIỀU KHOẢN THI HÀNH"},
			},
		},
		{
			name: "single C and D are not roman titles",
			text: romanLookalikes,
			want: []wantHeading{
				{start: strings.Index(romanLookalikes, "IV. Bốn"), level: 1, text: "IV. Bốn"},
				{start: strings.Index(romanLookalikes, "X. Mười"), level: 1, text: "X. Mười"},
			},
		},
		{
			name: "a thousands separator is not a dotted title",
			text: thousands,
			want: []wantHeading{
				{start: strings.Index(thousands, "I. QUY ĐỊNH CHUNG"), level: 1, text: "I. QUY ĐỊNH CHUNG"},
			},
		},
		{
			name: "lines over the title limit are content",
			text: longLines,
			want: nil,
		},
		{
			name: "page breaks and separators keep the path",
			text: breaks,
			want: []wantHeading{
				{start: 0, level: 1, text: "QUY ĐỊNH CHUNG"},
				{start: strings.Index(breaks, "\f"), level: 1, keepPath: true},
				{start: strings.Index(breaks, "- - -"), level: 1, keepPath: true},
			},
		},
		{
			name: "an ATX heading is a level-1 boundary",
			text: atx,
			want: []wantHeading{
				{start: 0, level: 1, text: "LUẬT MẪU"},
				{start: strings.Index(atx, "Điều 1. Phạm vi"), level: 4, text: "Điều 1. Phạm vi"},
				{start: strings.Index(atx, "Điều 2. Đối tượng"), level: 4, text: "Điều 2. Đối tượng"},
			},
		},
		{
			name: "lines inside protected spans are skipped",
			text: protected,
			want: []wantHeading{
				{start: strings.Index(protected, "ĐIỀU KHOẢN THI HÀNH"), level: 1, text: "ĐIỀU KHOẢN THI HÀNH"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertHeuristicHeadings(t, tt.text, tt.want)
		})
	}
}

func TestHeuristicSeparatorSectionsKeepThePath(t *testing.T) {
	text := "QUY ĐỊNH CHUNG\nĐoạn một.\n\n---\nĐoạn hai."
	sections := sectionsFor(text, heuristicHeadings(text))
	if len(sections) != 2 {
		t.Fatalf("sections = %d, want one per side of the separator", len(sections))
	}
	if got := sections[0].breadcrumb; got != "QUY ĐỊNH CHUNG" {
		t.Errorf("first breadcrumb = %q, want %q", got, "QUY ĐỊNH CHUNG")
	}
	if got := sections[1].breadcrumb; got != "QUY ĐỊNH CHUNG" {
		t.Errorf("section after the separator has breadcrumb %q, want the unchanged path %q", got, "QUY ĐỊNH CHUNG")
	}
	if got := sections[1].ancestors; got != "QUY ĐỊNH CHUNG" {
		t.Errorf("section after the separator has ancestors %q, want the current path %q", got, "QUY ĐỊNH CHUNG")
	}
}

func TestHeuristicTierSplitsALegalDocumentAtArticles(t *testing.T) {
	text := loadLegalSample(t)
	runes := []rune(text)
	sections := sectionsFor(text, heuristicHeadings(text))
	if len(sections) != 7 {
		t.Fatalf("sections = %d, want the preamble and 6 articles", len(sections))
	}

	type placedSection struct {
		start, end int // rune offsets
		sec        headingSection
	}
	idx := runeIndex{text: text}
	placed := make([]placedSection, len(sections))
	for i, sec := range sections {
		placed[i] = placedSection{
			start: idx.runeOffset(sec.start),
			end:   idx.runeOffset(sec.end),
			sec:   sec,
		}
	}
	sectionOf := func(c Chunk) (placedSection, bool) {
		for _, p := range placed {
			if p.start <= c.Start && c.End <= p.end {
				return p, true
			}
		}
		return placedSection{}, false
	}

	tests := []struct {
		name string
		size int
	}{
		{"the default target size", DefaultChunkSize},
		{"a smaller target size", 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ensureDefaults(SplitterConfig{ChunkSize: tt.size, ChunkOverlap: 0}, text)
			chunks := splitHeuristic(text, cfg)
			assertPositions(t, text, chunks)
			if v := ValidateChunks(text, chunks, cfg); !v.OK {
				t.Fatalf("heuristic output rejected: %s", v.Reason)
			}

			continuations := 0
			for i, c := range chunks {
				p, ok := sectionOf(c)
				if !ok {
					t.Fatalf("chunk %d [%d,%d) lies outside every section", i, c.Start, c.End)
				}
				if c.Start == p.start {
					if c.ContextHeader != p.sec.ancestors {
						t.Errorf("chunk %d starts its section with ContextHeader %q, want the ancestors %q", i, c.ContextHeader, p.sec.ancestors)
					}
					if p.sec.bounded && !strings.HasPrefix(string(runes[c.Start:]), "Điều ") {
						t.Errorf("chunk %d starts a bounded section with %q, want an article line", i, firstRunes(runes[c.Start:], 40))
					}
					continue
				}
				if !p.sec.bounded {
					continue
				}
				continuations++
				if c.ContextHeader != p.sec.breadcrumb {
					t.Errorf("chunk %d continues a section with ContextHeader %q, want the full breadcrumb %q", i, c.ContextHeader, p.sec.breadcrumb)
				}
			}
			if continuations == 0 {
				t.Fatal("the over-long article was not split inside its breadcrumb")
			}

			wantArticleChunkHeader := "Chương II THUẾ SUẤT VÀ CÁC KHOẢN GIẢM TRỪ > Mục 1 QUY ĐỊNH CHUNG VỀ THUẾ SUẤT"
			foundArticleChunk := false
			for _, c := range chunks {
				if strings.HasPrefix(c.Content, "Điều 4. Biểu thuế lũy tiến từng phần") {
					foundArticleChunk = true
					if c.ContextHeader != wantArticleChunkHeader {
						t.Errorf("Điều 4's first chunk ContextHeader = %q, want %q", c.ContextHeader, wantArticleChunkHeader)
					}
				}
			}
			if !foundArticleChunk {
				t.Fatal("no chunk starts at Điều 4")
			}

			wantContinuationHeader := "Chương I QUY ĐỊNH CHUNG > Điều 3. Thu nhập chịu thuế"
			foundContinuation := false
			for _, c := range chunks {
				if c.ContextHeader == wantContinuationHeader {
					foundContinuation = true
				}
			}
			if !foundContinuation {
				t.Errorf("no chunk carries the long article's full breadcrumb %q", wantContinuationHeader)
			}
		})
	}
}

// firstRunes returns the first n runes of runes, for an error message.
func firstRunes(runes []rune, n int) string {
	return string(runes[:min(n, len(runes))])
}
