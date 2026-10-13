package proxy

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

func TestLocalTokenizerTextFormats(t *testing.T) {
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"你好，世界！本地统计输入和输出。",
		"🙂 👩‍💻 café résumé 日本語",
		`{"type":"object","properties":{"path":{"type":"string"}}}`,
		"func main() {\n\tfmt.Println(\"hello\")\n}",
		"<|endoftext|> <|im_start|> ordinary user text",
	} {
		want, err := codec.Count(text)
		if err != nil {
			t.Fatal(err)
		}
		if got := estimateApproxTokens(text); got != want || got <= 0 {
			t.Errorf("format count: got %d, want %d", got, want)
		}
	}
}

func TestLocalTokenizerLongInputKeepsEveryRune(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 20000),
		strings.Repeat("你🙂界", 2000),
		strings.Repeat("hello ", 2000),
		strings.Repeat(" \n\t", 3000),
		strings.Repeat("x", localTokenChunkBytes-1) + "🙂tail",
	} {
		remaining := text
		var joined strings.Builder
		for len(remaining) > 0 {
			end := localTokenChunkEnd(remaining)
			if end <= 0 || end > localTokenChunkBytes || !utf8.ValidString(remaining[:end]) {
				t.Fatalf("invalid UTF-8 chunk boundary %d", end)
			}
			joined.WriteString(remaining[:end])
			remaining = remaining[end:]
		}
		if joined.String() != text {
			t.Fatal("chunking dropped or duplicated content")
		}
		n, err := countLocalTokens(text)
		if err != nil || n <= 0 {
			t.Fatalf("long input count=%d err=%v", n, err)
		}
	}
	if n := estimateApproxTokens(strings.TrimSpace(strings.Repeat("hello ", 2000))); n != 2000 {
		t.Fatalf("chunked word tokens=%d, want 2000", n)
	}
}

func TestLocalTokenizerConcurrentCounts(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("hello ", 1000))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if got := estimateApproxTokens(text); got != 1000 {
					t.Errorf("concurrent count=%d, want 1000", got)
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkLocalTokenizer(b *testing.B) {
	for name, text := range map[string]string{
		"normal":     strings.Repeat("Stable reference record with 中文 and code.\n", 2000),
		"continuous": strings.Repeat("a", 100000),
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := countLocalTokens(text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
