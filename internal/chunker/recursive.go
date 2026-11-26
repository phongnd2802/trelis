package chunker

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// separatorLevels are tried from coarsest to finest. A piece that does not
// fit the size cap at one level is split at the next; below the last level
// the piece is cut at the widest rune boundary that fits.
var separatorLevels = [][]string{
	{"\n\n", "\r\n\r\n"},
	{"\n"},
	{". ", "? ", "! ", "; ", "。", "？", "！"},
	{" "},
}

// splitRecursive splits text[start:end] into Chunks within the size cap,
// preferring coarse boundaries (paragraphs, then lines, sentences, words)
// and never cutting inside a protected span. A protected span larger than
// the cap becomes a Chunk of its own. Other tiers call it to split a
// section of their own that is over the cap.
func splitRecursive(text string, start, end int, protected spans, s Sizer) []Chunk {
	r := recursive{text: text, protected: protected, sizer: s}
	r.split(start, end, 0)
	return r.out
}

type recursive struct {
	text      string
	protected spans
	sizer     Sizer
	out       []Chunk
}

func (r *recursive) emit(start, end int) {
	start, end = trim(r.text, start, end)
	if start < end {
		r.out = append(r.out, Chunk{Content: r.text[start:end], Start: start, End: end})
	}
}

func (r *recursive) fits(start, end int) bool {
	start, end = trim(r.text, start, end)
	return r.sizer.Fits(r.text[start:end])
}

// split emits [start,end) as Chunks, splitting at separatorLevels[level]
// or finer.
func (r *recursive) split(start, end, level int) {
	if r.fits(start, end) {
		r.emit(start, end)
		return
	}
	if level == len(separatorLevels) {
		r.hardCut(start, end)
		return
	}
	cuts := r.cutsAfter(start, end, separatorLevels[level])
	if len(cuts) == 0 {
		r.split(start, end, level+1)
		return
	}
	// Join as many consecutive parts as fit into each Chunk; a part that
	// does not fit on its own is split at the next level.
	bounds := append(append([]int{start}, cuts...), end)
	for at := 0; at < len(bounds)-1; {
		next := r.widest(bounds, at)
		if next == at {
			r.split(bounds[at], bounds[at+1], level+1)
			next = at + 1
		} else {
			r.emit(bounds[at], bounds[next])
		}
		at = next
	}
}

// hardCut emits [start,end) in the widest pieces that fit, cutting at rune
// boundaries outside protected spans, for text with no separator left to
// split at. A protected span that does not fit is emitted whole.
func (r *recursive) hardCut(start, end int) {
	bounds := []int{start}
	for i := start; i < end; {
		_, size := utf8.DecodeRuneInString(r.text[i:end])
		i += size
		if i == end || !r.protected.inside(i) {
			bounds = append(bounds, i)
		}
	}
	for at := 0; at < len(bounds)-1; {
		// Take at least one piece, so the loop makes progress.
		next := max(r.widest(bounds, at), at+1)
		r.emit(bounds[at], bounds[next])
		at = next
	}
}

// widest returns the largest j > at such that text[bounds[at]:bounds[j]]
// fits, or at if none does. It gallops to bracket j and then binary
// searches, so each probe measures about one Chunk of text and the whole
// split stays near-linear.
func (r *recursive) widest(bounds []int, at int) int {
	lo, step := at, 1
	for lo+step < len(bounds) && r.fits(bounds[at], bounds[lo+step]) {
		lo += step
		step *= 2
	}
	hi := min(lo+step, len(bounds)) - 1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if r.fits(bounds[at], bounds[mid]) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// cutsAfter returns, in order, each position in (start,end) right after an
// occurrence of one of seps that does not fall inside a protected span.
func (r *recursive) cutsAfter(start, end int, seps []string) []int {
	var cuts []int
	for _, sep := range seps {
		for i := start; ; {
			j := strings.Index(r.text[i:end], sep)
			if j < 0 {
				break
			}
			i += j + len(sep)
			if i < end && !r.protected.inside(i) {
				cuts = append(cuts, i)
			}
		}
	}
	slices.Sort(cuts)
	return slices.Compact(cuts)
}
