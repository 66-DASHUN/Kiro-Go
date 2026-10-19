package anthropictokenizer

import (
	"strings"
	"sync"
	"testing"
)

func TestEmbeddedVocabularyInitializes(t *testing.T) {
	if _, err := getTokenizer(); err != nil {
		t.Fatalf("must not silently fall back to chars/4: %v", err)
	}
	ranks, err := parseBPERanks("! 5 IQ== Ig==", 0)
	if err != nil || ranks["!"] != 5 || ranks["\""] != 6 {
		t.Fatalf("rank header: %v %v", ranks, err)
	}
}

func TestAnthropicReferenceGoldenCounts(t *testing.T) {
	// Independently checked against @anthropic-ai/tokenizer 0.0.4.
	for text, want := range map[string]int{"hello world!": 3, "™": 1, "ϰ": 1, "<EOT>": 1, "你好，世界！本地统计输入和输出。": 13, "🙂 👩‍💻 café résumé 日本語": 19} {
		if got := CountTokens(text); got != want {
			t.Errorf("%q: %d want %d", text, got, want)
		}
	}
}

func TestReferenceLongAndConcurrentInput(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("hello ", 100000))
	if got := CountTokens(text); got != 100000 {
		t.Fatalf("long input lost tokens: %d", got)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if CountTokens("你好，世界！本地统计输入和输出。") != 13 {
					t.Error("unstable concurrent count")
				}
			}
		}()
	}
	wg.Wait()
}

func TestBoundedUninterruptedInput(t *testing.T) {
	for _, unit := range []string{"a", "中", "🙂"} {
		text := strings.Repeat(unit, 32768)
		got := CountTokens(text)
		if got <= 0 {
			t.Fatal("lost long text")
		}
		if end := boundedChunkEnd(text); end > 4096 || end < 2048 {
			t.Fatalf("unbounded chunk %d", end)
		}
	}
}
