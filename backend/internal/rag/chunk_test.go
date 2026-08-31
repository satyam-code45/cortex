package rag_test

import (
	"fmt"
	"strings"
	"testing"

	"cortex/internal/rag"
)

// The chunker.
//
// Four properties are required:
// a short document produces one chunk, a long one carries the right overlap,
// paragraph boundaries are preferred, and the token caps are exact. "Exact" is
// exactness against rag.EstimateTokens — by design that function is the
// single definition of "token" the chunker enforces its caps against, precisely
// so this test can be written at all.
//
// Every case therefore checks the same invariants (cap, index ordering, no lost
// words) and adds whatever is specific to it.

// ---------------------------------------------------------------------------
// EstimateTokens — the yardstick everything else is measured against (A5)
// ---------------------------------------------------------------------------

func TestEstimateTokensIsCeilingOfRunesOverFour(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "empty", text: "", want: 0},
		{name: "one rune", text: "a", want: 1},
		{name: "exactly one token", text: "abcd", want: 1},
		{name: "one rune over", text: "abcde", want: 2},
		{name: "two whole tokens", text: "abcdefgh", want: 2},
		// Runes, not bytes: a multi-byte character is one character to a reader
		// and must be one character to the estimator, or the cap is wrong by a
		// factor of three on non-ASCII text.
		{name: "multi-byte runes count once each", text: "héllo", want: 2},
		{name: "whitespace counts", text: "a b c d e f g h", want: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := rag.EstimateTokens(tt.text); got != tt.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ChunkText
// ---------------------------------------------------------------------------

// paragraph builds a paragraph of n distinct seven-character words, so that a
// chunk boundary can never be confused for a coincidence and a word that was
// cut in half is visible in the failure output.
func paragraph(tag string, words int) string {
	parts := make([]string, 0, words)
	for i := range words {
		parts = append(parts, fmt.Sprintf("%s%02d", tag, i))
	}
	return strings.Join(parts, " ")
}

// document joins paragraphs the way a real source does.
func document(paragraphs ...string) string {
	return strings.Join(paragraphs, "\n\n")
}

func TestChunkText(t *testing.T) {
	t.Parallel()

	// Two paragraphs of 71 characters each (9 words of 7 chars + 8 spaces).
	shortDoc := document("Atlas Q2 goals are set.", "The payments sandbox is down.")

	// A body whose paragraphs fit a chunk individually but not in threes.
	p1, p2, p3 := paragraph("alpha", 9), paragraph("bravo", 9), paragraph("delta", 9)

	// Six paragraphs, none of which will share a chunk once the overlap
	// allowance is reserved.
	longDoc := document(
		paragraph("alpha", 9), paragraph("bravo", 9), paragraph("delta", 9),
		paragraph("gamma", 9), paragraph("omega", 9), paragraph("sigma", 9),
	)

	tests := []struct {
		name          string
		text          string
		maxTokens     int
		overlapTokens int

		// capTokens is the cap every chunk must satisfy, after the chunker's own
		// defaulting of maxTokens.
		capTokens int
		// wantChunks is the exact chunk count; -1 leaves it unchecked.
		wantChunks int
		// wantExact, when set, is the single chunk's expected content.
		wantExact string
		// wantOverlapTokens > 0 turns on the overlap assertions.
		wantOverlapTokens int
		// wantWholeWordOverlap additionally demands that the overlap carries the
		// previous chunk's last word intact.
		wantWholeWordOverlap bool
		// wantParagraphsIntact must each appear verbatim in some chunk.
		wantParagraphsIntact []string
	}{
		{
			name:          "short document is a single chunk",
			text:          shortDoc,
			maxTokens:     rag.DefaultChunkTokens,
			overlapTokens: rag.DefaultOverlapTokens,
			capTokens:     rag.DefaultChunkTokens,
			wantChunks:    1,
			wantExact:     shortDoc,
		},
		{
			name:          "empty text yields no chunks",
			text:          "",
			maxTokens:     rag.DefaultChunkTokens,
			overlapTokens: rag.DefaultOverlapTokens,
			capTokens:     rag.DefaultChunkTokens,
			wantChunks:    0,
		},
		{
			name:          "whitespace-only text yields no chunks",
			text:          "   \n\n \t \r\n  ",
			maxTokens:     rag.DefaultChunkTokens,
			overlapTokens: rag.DefaultOverlapTokens,
			capTokens:     rag.DefaultChunkTokens,
			wantChunks:    0,
		},
		{
			name:          "windows line endings are normalized",
			text:          "First paragraph.\r\n\r\nSecond paragraph.",
			maxTokens:     rag.DefaultChunkTokens,
			overlapTokens: rag.DefaultOverlapTokens,
			capTokens:     rag.DefaultChunkTokens,
			wantChunks:    1,
			wantExact:     "First paragraph.\n\nSecond paragraph.",
		},
		{
			// Paragraph-boundary preference: with room for two of these
			// paragraphs but not three, the split lands between paragraphs and
			// every paragraph survives intact.
			name:                 "paragraph boundaries are preferred over fixed windows",
			text:                 document(p1, p2, p3),
			maxTokens:            40,
			overlapTokens:        0,
			capTokens:            40,
			wantChunks:           2,
			wantParagraphsIntact: []string{p1, p2, p3},
		},
		{
			name:                 "long document overlaps consecutive chunks",
			text:                 longDoc,
			maxTokens:            40,
			overlapTokens:        10,
			capTokens:            40,
			wantChunks:           6,
			wantOverlapTokens:    10,
			wantWholeWordOverlap: true,
		},
		{
			// A paragraph too large to fit alone is split at word boundaries,
			// and the overlap still holds across those splits.
			name:                 "over-long paragraph splits at word boundaries",
			text:                 paragraph("solo", 60),
			maxTokens:            40,
			overlapTokens:        10,
			capTokens:            40,
			wantChunks:           -1,
			wantOverlapTokens:    10,
			wantWholeWordOverlap: true,
		},
		{
			// The cap is the whole point: a chunk over the embedding model's
			// input limit fails the run. Squeeze it until only one word fits.
			name:              "tiny cap is still exact",
			text:              paragraph("micro", 12),
			maxTokens:         4,
			overlapTokens:     1,
			capTokens:         4,
			wantChunks:        -1,
			wantOverlapTokens: 1,
		},
		{
			name:          "non-positive maxTokens falls back to the default",
			text:          shortDoc,
			maxTokens:     0,
			overlapTokens: -5,
			capTokens:     rag.DefaultChunkTokens,
			wantChunks:    1,
			wantExact:     shortDoc,
		},
		{
			// An overlap as large as the chunk would make no forward progress;
			// clamping it must still terminate and still respect the cap.
			name:          "overlap as large as the chunk still makes progress",
			text:          paragraph("clamp", 40),
			maxTokens:     20,
			overlapTokens: 20,
			capTokens:     20,
			wantChunks:    -1,
		},
		{
			name:          "spec defaults: ~500 tokens with 50 overlap",
			text:          document(paragraph("plan", 80), paragraph("risk", 80), paragraph("goal", 80)),
			maxTokens:     rag.DefaultChunkTokens,
			overlapTokens: rag.DefaultOverlapTokens,
			capTokens:     rag.DefaultChunkTokens,
			wantChunks:    -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chunks := rag.ChunkText(tt.text, tt.maxTokens, tt.overlapTokens)

			if tt.wantChunks >= 0 && len(chunks) != tt.wantChunks {
				t.Fatalf("chunks = %d, want %d\n%s", len(chunks), tt.wantChunks, describeChunks(chunks))
			}
			if tt.wantExact != "" {
				if len(chunks) != 1 {
					t.Fatalf("chunks = %d, want exactly 1", len(chunks))
				}
				if chunks[0].Content != tt.wantExact {
					t.Errorf("chunk content = %q, want %q", chunks[0].Content, tt.wantExact)
				}
			}

			// Index ordering: the chunk index is what document_chunks is keyed
			// by, so a gap or a repeat is a unique-constraint violation waiting
			// to happen.
			for i, chunk := range chunks {
				if chunk.Index != i {
					t.Errorf("chunks[%d].Index = %d, want %d", i, chunk.Index, i)
				}
				if strings.TrimSpace(chunk.Content) == "" {
					t.Errorf("chunks[%d] is empty", i)
				}
			}

			// The exact cap, measured with the chunker's own definition of a
			// token (A5). Overlap included — it is part of the chunk that gets
			// embedded.
			for i, chunk := range chunks {
				if got := rag.EstimateTokens(chunk.Content); got > tt.capTokens {
					t.Errorf("chunks[%d] is %d estimated tokens, want <= %d\ncontent: %q",
						i, got, tt.capTokens, chunk.Content)
				}
			}

			// Nothing may be dropped: the source's word sequence has to survive
			// somewhere in the chunk stream, in order.
			assertWordsPreserved(t, tt.text, chunks)

			for _, want := range tt.wantParagraphsIntact {
				if !containsChunkText(chunks, want) {
					t.Errorf("paragraph was split across chunks; no chunk contains %q\n%s",
						want, describeChunks(chunks))
				}
			}

			if tt.wantOverlapTokens > 0 {
				assertOverlap(t, chunks, tt.wantOverlapTokens, tt.wantWholeWordOverlap)
			}

			// Progress: a chunker that repeats itself never terminates on real
			// input, and the clamped-overlap case exists to catch exactly that.
			for i := 1; i < len(chunks); i++ {
				if chunks[i].Content == chunks[i-1].Content {
					t.Fatalf("chunks[%d] repeats chunks[%d] verbatim: no forward progress", i, i-1)
				}
			}
		})
	}
}

// assertOverlap checks that each chunk after the first begins with a non-empty
// tail of its predecessor, sized within the overlap allowance.
//
// The shared text is found as the longest prefix of the chunk that is both a
// suffix of the previous chunk and inside the allowance — which is precisely
// what "50 tokens of overlap" means once the words have been snapped to
// boundaries.
func assertOverlap(t *testing.T, chunks []rag.Chunk, overlapTokens int, wholeWord bool) {
	t.Helper()
	if len(chunks) < 2 {
		t.Fatalf("overlap cannot be asserted on %d chunk(s)", len(chunks))
	}

	for i := 1; i < len(chunks); i++ {
		previous, current := chunks[i-1].Content, chunks[i].Content

		shared := ""
		runes := []rune(current)
		for j := 1; j <= len(runes); j++ {
			candidate := string(runes[:j])
			if rag.EstimateTokens(candidate) > overlapTokens {
				break
			}
			if strings.HasSuffix(previous, candidate) {
				shared = candidate
			}
		}

		if shared == "" {
			t.Errorf("chunks[%d] carries no overlap from chunks[%d]\nprevious tail: %q\ncurrent head: %q",
				i, i-1, tailRunes(previous, 60), headRunes(current, 60))
			continue
		}
		if got := rag.EstimateTokens(shared); got > overlapTokens {
			t.Errorf("chunks[%d] overlap is %d estimated tokens, want <= %d", i, got, overlapTokens)
		}
		if wholeWord {
			// The overlap exists so a sentence split across a boundary is still
			// retrievable; carrying half a word forward does not achieve that.
			last := lastWord(previous)
			if last != "" && !strings.Contains(shared, last) {
				t.Errorf("chunks[%d] overlap %q does not carry the previous chunk's last word %q",
					i, shared, last)
			}
		}
	}
}

// assertWordsPreserved checks that every whitespace-separated word of the source
// appears, in order, somewhere in the chunk stream.
func assertWordsPreserved(t *testing.T, source string, chunks []rag.Chunk) {
	t.Helper()

	want := strings.Fields(source)
	var got []string
	for _, chunk := range chunks {
		got = append(got, strings.Fields(chunk.Content)...)
	}

	cursor := 0
	for _, word := range want {
		for cursor < len(got) && got[cursor] != word {
			cursor++
		}
		if cursor == len(got) {
			t.Fatalf("word %q from the source appears in no chunk (or out of order)\n%s",
				word, describeChunks(chunks))
		}
		cursor++
	}
}

func containsChunkText(chunks []rag.Chunk, want string) bool {
	for _, chunk := range chunks {
		if strings.Contains(chunk.Content, want) {
			return true
		}
	}
	return false
}

func lastWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func headRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n]) + "…"
}

func tailRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return "…" + string(runes[len(runes)-n:])
}

func describeChunks(chunks []rag.Chunk) string {
	var b strings.Builder
	for _, chunk := range chunks {
		fmt.Fprintf(&b, "  [%d] %d tokens: %q\n",
			chunk.Index, rag.EstimateTokens(chunk.Content), headRunes(chunk.Content, 90))
	}
	return b.String()
}
