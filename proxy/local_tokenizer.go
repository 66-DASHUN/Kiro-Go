package proxy

import (
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// The embedded o200k_base vocabulary is a local approximation, not Claude's
// proprietary tokenizer. No vocabulary download or external counting API runs.
var localTokenizer struct {
	once  sync.Once
	codec tokenizer.Codec
	err   error
}

// Bound the merge work for long uninterrupted text: the codec's merge loop is
// quadratic in each piece. Splitting can slightly change counts at boundaries.
const localTokenChunkBytes = 4096

func countLocalTokens(text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	localTokenizer.once.Do(func() {
		localTokenizer.codec, localTokenizer.err = tokenizer.Get(tokenizer.O200kBase)
	})
	if localTokenizer.err != nil {
		return 0, localTokenizer.err
	}

	total := 0
	for len(text) > 0 {
		end := localTokenChunkEnd(text)
		n, err := localTokenizer.codec.Count(text[:end])
		if err != nil {
			return 0, err
		}
		total += n
		text = text[end:]
	}
	return total, nil
}

func localTokenChunkEnd(text string) int {
	if len(text) <= localTokenChunkBytes {
		return len(text)
	}
	end := localTokenChunkBytes
	for !utf8.RuneStart(text[end]) {
		end--
	}
	// Leave the separator at the start of the next chunk so common leading-space
	// word tokens keep their shape. Prefer a separator in the latter half only.
	for pos := end; pos > end/2; {
		r, size := utf8.DecodeLastRuneInString(text[:pos])
		pos -= size
		if unicode.IsSpace(r) && pos >= end/2 {
			return pos
		}
	}
	return end
}
