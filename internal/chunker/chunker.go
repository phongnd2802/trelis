// Package chunker splits a Document's text into Chunks.
//
// A Chunker runs a chain of tiers. Each tier proposes Chunks, a Validator
// checks them, and rejected Chunks fall through to the next tier. The last
// tier is always the recursive tier, whose Chunks are returned even if the
// Validator rejects them, so non-blank text always yields Chunks.
//
// Every tier respects the same two rules: no Chunk is over the size cap,
// which is derived from a token limit, and no Chunk boundary falls inside a
// protected span (a table, fenced code block or math block).
package chunker

import (
	"errors"
	"slices"
	"strings"
	"unicode"
)

// Chunk is a contiguous piece of the source text. Content is exactly
// text[Start:End], with Start and End byte offsets into the text given to
// Chunker.Chunk. Context a tier adds that is not at those offsets (a
// heading breadcrumb, a table header carried into a continuation Chunk)
// belongs in a field of its own, so offsets stay exact.
type Chunk struct {
	Content string
	Start   int
	End     int
}

// Config sets the size of the Chunks a Chunker produces.
type Config struct {
	// TokenLimit is the most tokens the consumer of a Chunk (usually the
	// embedding model) accepts. It is the size cap: no Chunk counts more
	// than TokenLimit tokens, except a protected span that is larger on
	// its own.
	TokenLimit int
	// CountTokens counts tokens for the size cap. Nil means
	// EstimateTokens; pass the embedding model's tokenizer for an exact
	// cap.
	CountTokens TokenCounter
}

// Sizer measures text against the size cap.
type Sizer struct {
	limit int
	count TokenCounter
}

// Limit is the size cap in tokens.
func (s Sizer) Limit() int { return s.limit }

// Tokens counts the tokens in text.
func (s Sizer) Tokens(text string) int { return s.count(text) }

// Fits reports whether text is within the size cap.
func (s Sizer) Fits(text string) bool { return s.count(text) <= s.limit }

// Input is the text a tier splits, with what the Chunker found in it.
type Input struct {
	Text string
	// Protected is ProtectedSpans(Text).
	Protected []Span
}

// Tier is one way of splitting text into Chunks.
type Tier interface {
	// Name identifies the tier in Result.Tier and Result.Rejected.
	Name() string
	// Split proposes Chunks for in.Text. They need not be valid: the
	// Validator decides, and invalid Chunks fall through to the next tier.
	Split(in Input, size Sizer) []Chunk
}

// Recursive is the recursive tier: it splits at paragraphs, then lines,
// sentences and words, then rune boundaries, until each Chunk fits. It
// is always the last tier of a Chunker.
var Recursive Tier = recursiveTier{}

type recursiveTier struct{}

func (recursiveTier) Name() string { return "recursive" }

func (recursiveTier) Split(in Input, size Sizer) []Chunk {
	return splitRecursive(in.Text, 0, len(in.Text), in.Protected, size)
}

// Rejection is a tier whose Chunks the Validator rejected.
type Rejection struct {
	Tier   string
	Reason error
}

// Result is the output of one Chunker.Chunk call.
type Result struct {
	Chunks []Chunk
	// Tier names the tier that produced Chunks.
	Tier string
	// Rejected lists, in order, the tiers whose Chunks were rejected.
	Rejected []Rejection
}

// Option configures a Chunker beyond its Config.
type Option func(*Chunker)

// WithTiers sets the tiers tried, in order, before the recursive tier. The
// recursive tier always runs last, so listing it here has no effect.
func WithTiers(tiers ...Tier) Option {
	return func(c *Chunker) {
		c.tiers = slices.DeleteFunc(slices.Clone(tiers), func(t Tier) bool { return t == Recursive })
	}
}

// WithValidator replaces Validate as the Validator.
func WithValidator(v Validator) Option {
	return func(c *Chunker) { c.validate = v }
}

// Chunker splits text into Chunks. It is safe for concurrent use as long
// as its tiers are.
type Chunker struct {
	sizer    Sizer
	tiers    []Tier
	validate Validator
}

// New returns a Chunker for cfg. With no options it runs the recursive tier
// alone, checked by Validate.
func New(cfg Config, opts ...Option) (*Chunker, error) {
	if cfg.TokenLimit <= 0 {
		return nil, errors.New("chunker: TokenLimit must be positive")
	}
	count := cfg.CountTokens
	if count == nil {
		count = EstimateTokens
	}
	c := &Chunker{sizer: Sizer{limit: cfg.TokenLimit, count: count}, validate: Validate}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Chunk splits text into Chunks. Blank text has none.
func (c *Chunker) Chunk(text string) Result {
	if strings.TrimSpace(text) == "" {
		return Result{}
	}
	in := Input{Text: text, Protected: ProtectedSpans(text)}
	var res Result
	chain := append(slices.Clip(c.tiers), Recursive)
	for i, tier := range chain {
		res.Chunks, res.Tier = tier.Split(in, c.sizer), tier.Name()
		err := c.validate(in, res.Chunks, c.sizer)
		if err == nil {
			break
		}
		res.Rejected = append(res.Rejected, Rejection{Tier: tier.Name(), Reason: err})
		if i == len(chain)-1 {
			break // the recursive tier's Chunks are kept anyway
		}
	}
	return res
}

// trim narrows [start,end) of text to exclude surrounding whitespace.
func trim(text string, start, end int) (int, int) {
	s := text[start:end]
	left := strings.TrimLeftFunc(s, unicode.IsSpace)
	start += len(s) - len(left)
	end = start + len(strings.TrimRightFunc(left, unicode.IsSpace))
	return start, end
}
