// Package rag holds the indexed half of retrieval: the pipeline that turns
// source documents into embedded chunks in pgvector, and the tool that searches
// them.
//
// The division of labour with the live tools is the point (idea.md §11). A
// question about current state — who owns this ticket, what is its status — must
// go to Jira, because an index is stale the moment it is written. A question
// about what was written, argued or decided is better served by semantic search
// over everything, because the agent cannot guess the keywords that happen to
// appear in a two-month-old retro. Neither replaces the other.
package rag

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	// DefaultChunkTokens is the target size of one chunk.
	//
	// ~500 tokens is small enough that a retrieved chunk is mostly about one
	// thing (which is what makes cosine similarity meaningful) and large enough
	// to carry a whole argument rather than a fragment of one.
	DefaultChunkTokens = 500

	// DefaultOverlapTokens is how much of the previous chunk each chunk repeats.
	//
	// Overlap exists because chunk boundaries are arbitrary with respect to
	// meaning: a sentence that names the vendor and a sentence that gives the
	// date can land either side of a split, and neither chunk would then answer
	// "when did the vendor slip?". 10% is the usual trade between that risk and
	// paying to embed the same text twice.
	DefaultOverlapTokens = 50

	// charsPerToken is the estimator's ratio — see EstimateTokens.
	charsPerToken = 4
)

// Chunk is one embeddable slice of a document.
type Chunk struct {
	Index   int
	Content string
}

// EstimateTokens approximates the token count of a string.
//
// It is an estimate, and deliberately a crude one: the locked stack (CLAUDE.md)
// carries no tokenizer, and pulling in tiktoken to size a chunk would be a
// dependency bought for a heuristic. Four characters per token is the standard
// rule of thumb for English text and is what the rest of this codebase already
// assumes (see DefaultMaxToolContentChars in internal/agent).
//
// What matters for correctness is not that it is accurate but that it is the
// single definition of "token" the chunker enforces its caps against — so a
// chunk that this function says fits, fits.
func EstimateTokens(text string) int {
	runes := len([]rune(text))
	if runes == 0 {
		return 0
	}
	return (runes + charsPerToken - 1) / charsPerToken
}

// ChunkText splits text into overlapping chunks of at most maxTokens.
//
// Paragraphs are the preferred boundary: a blank line in a Notion page or an
// email is an author's own statement about where one idea ends, and splitting
// there produces chunks that are about a single thing far more often than any
// fixed-width window does. Whole paragraphs are packed together while they fit;
// a paragraph too large to fit alone is split at word boundaries.
//
// Every returned chunk satisfies EstimateTokens(chunk.Content) <= maxTokens,
// overlap included. That is achieved by reserving the overlap allowance up front
// — the budget for new material is maxTokens minus the overlap — rather than by
// adding the overlap on top of a full chunk and hoping. The first chunk has no
// overlap and so is slightly under-filled; paying that to make the cap
// unconditional is a good trade, because a chunk that exceeds the embedding
// model's input limit fails the whole indexing run.
//
// overlapTokens is clamped below maxTokens: an overlap as large as the chunk
// would make no forward progress.
func ChunkText(text string, maxTokens, overlapTokens int) []Chunk {
	if maxTokens <= 0 {
		maxTokens = DefaultChunkTokens
	}
	if overlapTokens < 0 {
		overlapTokens = 0
	}
	if overlapTokens >= maxTokens {
		overlapTokens = maxTokens / 2
	}

	normalized := strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if normalized == "" {
		return nil
	}

	maxChars := maxTokens * charsPerToken
	overlapChars := overlapTokens * charsPerToken

	// bodyMax is what a chunk may hold besides its overlap prefix. Every segment
	// below is cut to fit it, so packing never has to reason about the prefix.
	//
	// It cannot go non-positive: the clamp above holds overlapTokens strictly
	// below maxTokens, so overlapChars is at most maxChars/2 and at least
	// charsPerToken remains. A defensive `if bodyMax <= 0 { bodyMax = maxChars }`
	// used to sit here and was worse than nothing — it was unreachable, and had
	// it ever fired it would have restored the full width to the body while the
	// overlap was still prepended, silently breaking the one guarantee this
	// function documents unconditionally.
	bodyMax := maxChars - overlapChars - separatorRunes

	var (
		chunks       []Chunk
		current      []string
		currentChars int
		carry        string
	)

	flush := func() {
		if len(current) == 0 {
			return
		}
		content := strings.Join(current, paragraphSeparator)
		if carry != "" {
			content = carry + paragraphSeparator + content
		}
		chunks = append(chunks, Chunk{Index: len(chunks), Content: content})
		carry = tailWords(content, overlapChars)
		current = nil
		currentChars = 0
	}

	for _, segment := range segments(normalized, bodyMax) {
		size := len([]rune(segment))
		separator := 0
		if len(current) > 0 {
			separator = separatorRunes
		}
		if currentChars+separator+size > bodyMax {
			flush()
			separator = 0
		}
		current = append(current, segment)
		currentChars += separator + size
	}
	flush()

	return chunks
}

// paragraphSeparator is what packed segments are joined by. It is the same blank
// line they were split on, so a chunk reads as the source did.
const paragraphSeparator = "\n\n"

// separatorRunes is the separator's length in RUNES, which is the unit every
// budget here is measured in. Spelled out rather than written as
// len(paragraphSeparator): that is a byte count, and it agrees with the rune
// count only because this separator happens to be ASCII. The cap arithmetic
// would quietly start over-filling chunks the day the separator gained a
// multi-byte character.
var separatorRunes = len([]rune(paragraphSeparator))

// blankLine matches a paragraph break, tolerating the trailing spaces that real
// documents are full of.
var blankLine = regexp.MustCompile(`\n[ \t]*\n`)

// segments breaks text into pieces that each fit in maxChars.
//
// A paragraph short enough to fit is one segment, which is what gives the
// chunker its paragraph-boundary preference. A longer one is broken at word
// boundaries: a word cut in half embeds as neither word, and the retrieved chunk
// reads as corrupted text to whoever is shown it.
func segments(text string, maxChars int) []string {
	var out []string
	for _, block := range blankLine.Split(text, -1) {
		paragraph := strings.TrimSpace(block)
		if paragraph == "" {
			continue
		}
		if len([]rune(paragraph)) <= maxChars {
			out = append(out, paragraph)
			continue
		}
		out = append(out, splitWords(paragraph, maxChars)...)
	}
	return out
}

// splitWords breaks an over-long paragraph at whitespace.
//
// A single "word" longer than the budget — a URL, a base64 attachment, a line of
// minified JSON — is hard-cut rather than emitted oversized. Mangling a blob is
// harmless; a chunk over the embedding model's input limit fails the API call
// and takes the whole indexing run with it.
func splitWords(paragraph string, maxChars int) []string {
	var (
		pieces  []string
		current strings.Builder
		count   int
	)
	emit := func() {
		if count > 0 {
			pieces = append(pieces, current.String())
			current.Reset()
			count = 0
		}
	}

	for _, word := range strings.FieldsFunc(paragraph, unicode.IsSpace) {
		for _, part := range cutRunes(word, maxChars) {
			size := len([]rune(part))
			separator := 0
			if count > 0 {
				separator = 1
			}
			if count+separator+size > maxChars {
				emit()
				separator = 0
			}
			if separator > 0 {
				current.WriteByte(' ')
			}
			current.WriteString(part)
			count += separator + size
		}
	}
	emit()
	return pieces
}

// cutRunes slices s into runs of at most n runes, returning s unchanged when it
// already fits.
func cutRunes(s string, n int) []string {
	runes := []rune(s)
	if len(runes) <= n {
		return []string{s}
	}
	var out []string
	for len(runes) > n {
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

// tailWords returns the last chars runes of text, snapped forward to a word
// boundary so the overlap does not begin mid-word.
//
// Forward, not backward: snapping backward would grow the overlap past the
// allowance reserved for it, and the cap would stop holding.
func tailWords(text string, chars int) string {
	if chars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= chars {
		return strings.TrimSpace(text)
	}

	tail := string(runes[len(runes)-chars:])
	if idx := strings.IndexFunc(tail, unicode.IsSpace); idx >= 0 {
		tail = tail[idx:]
	}
	return strings.TrimSpace(tail)
}
