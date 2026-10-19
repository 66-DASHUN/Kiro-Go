package proxy

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type outputBudgetContextKey struct{}

// outputBudget limits semantic output, independently of provider billing.
// Keep the existing bounded codec independent of billable usage estimation. Signatures, thinking tags and protocol envelopes cost zero.
type outputBudget struct {
	limit, used int
	stopped     bool
	truncated   bool
	channels    [2]outputBudgetChannel
}

type outputBudgetChannel struct {
	tail   string
	tokens int
}

func newOutputBudget(limit int) *outputBudget {
	if limit <= 0 {
		return nil
	}
	return &outputBudget{limit: limit}
}

func (b *outputBudget) consume(text string, thinking bool) string {
	if b == nil {
		return text
	}
	if b.stopped || text == "" {
		return ""
	}
	idx := 0
	if thinking {
		idx = 1
	}
	c := &b.channels[idx]
	remaining := b.limit - b.used
	count := func(s string) int { return estimateBudgetTokens(c.tail+s) - c.tokens }
	accepted := text
	if count(text) > remaining {
		// Token counts need not be monotone at BPE boundaries. Examine every
		// rune boundary in this bounded batch rather than binary-searching.
		end := 0
		for pos := 0; pos < len(text); {
			_, width := utf8.DecodeRuneInString(text[pos:])
			pos += width
			if count(text[:pos]) <= remaining {
				end = pos
			}
		}
		accepted = text[:end]
		b.stopped = true
		b.truncated = true
	}
	newTokens := estimateBudgetTokens(c.tail + accepted)
	b.used += newTokens - c.tokens
	c.tail += accepted
	c.tokens = newTokens
	// Keep precisely the tokenizer's final, unstable chunk. Once another
	// chunk exists, earlier chunks can never change as more text arrives.
	for len(c.tail) > localTokenChunkBytes {
		end := localTokenChunkEnd(c.tail)
		c.tokens -= estimateBudgetTokens(c.tail[:end])
		c.tail = c.tail[end:]
	}
	if b.used >= b.limit {
		b.stopped = true
	}
	return accepted
}

func (b *outputBudget) tool(tu KiroToolUse) bool {
	if b == nil {
		return true
	}
	if b.stopped {
		return false
	}
	input := tu.Input
	if input == nil {
		input = map[string]interface{}{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		b.stopped = true
		return false
	}
	cost := estimateBudgetTokens(tu.Name) + estimateBudgetTokens(string(raw))
	if cost > b.limit-b.used {
		b.stopped = true
		b.truncated = true
		return false
	}
	b.used += cost
	if b.used >= b.limit {
		b.stopped = true
	}
	return true
}

// Coalesce tiny network fragments into deterministic batches. Counting a
// bounded tokenizer tail per batch keeps work linear in total response size;
// counting every entire accumulated response would become quadratic.
const outputBudgetBatchBytes = 128

func (b *outputBudget) text(text string, thinking bool) string {
	var accepted strings.Builder
	for len(text) > 0 && (b == nil || !b.stopped) {
		end := min(len(text), outputBudgetBatchBytes)
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		accepted.WriteString(b.consume(text[:end], thinking))
		text = text[end:]
	}
	return accepted.String()
}

func wrapOutputBudget(callback *KiroStreamCallback, budget *outputBudget) *KiroStreamCallback {
	if budget == nil {
		return callback
	}
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	wrapped := *callback
	var splitter thinkingTagSplitter
	var pending string
	var pendingEmitted int
	kind := -1 // answer, native reasoning, inline reasoning
	emit := func(text string) {
		if text == "" || callback.OnText == nil {
			return
		}
		if kind == 2 {
			callback.OnText(thinkingOpenTag+text+thinkingCloseTag, false)
			return
		}
		callback.OnText(text, kind == 1)
	}
	flush := func() {
		if pending != "" {
			accepted := budget.consume(pending, kind > 0)
			if len(accepted) > pendingEmitted {
				emit(accepted[pendingEmitted:])
			}
			pending = ""
			pendingEmitted = 0
		}
	}
	push := func(text string, nextKind int) {
		if budget.stopped {
			if nextKind == 1 && text != "" {
				budget.truncated = true
			}
			return
		}
		if kind != nextKind {
			flush()
			kind = nextKind
		}
		pending += text
		for len(pending) >= outputBudgetBatchBytes && !budget.stopped {
			end := outputBudgetBatchBytes
			for end > 0 && end < len(pending) && !utf8.RuneStart(pending[end]) {
				end--
			}
			accepted := budget.consume(pending[:end], kind > 0)
			if len(accepted) > pendingEmitted {
				emit(accepted[pendingEmitted:])
			}
			pending = pending[end:]
			pendingEmitted = 0
		}
		// Byte-pair encoding cannot require more tokens than UTF-8 bytes.
		// With ample headroom, release this suffix immediately and account
		// for it at the next deterministic boundary. Reserve another batch
		// for any tokenizer boundary change; near the cap, hold the suffix.
		if !budget.stopped && len(pending) > pendingEmitted && budget.limit-budget.used >= len(pending)+outputBudgetBatchBytes {
			emit(pending[pendingEmitted:])
			pendingEmitted = len(pending)
		}
	}
	answer := func(s string) { push(s, 0) }
	inline := func(s string) { push(s, 2) }
	wrapped.OnText = func(text string, thinking bool) {
		if thinking {
			splitter.Flush(answer, inline)
			push(text, 1)
		} else {
			splitter.Push(text, answer, inline)
		}
	}
	wrapped.OnThinkingSignature = func(signature string) {
		flush()
		// A signature authenticates the complete original reasoning block.
		// Once limited, never associate it with shortened reasoning.
		if !budget.truncated && callback.OnThinkingSignature != nil {
			callback.OnThinkingSignature(signature)
		}
	}
	wrapped.OnToolUse = func(tu KiroToolUse) {
		splitter.Flush(answer, inline)
		flush()
		if budget.tool(tu) && callback.OnToolUse != nil {
			callback.OnToolUse(tu)
		}
	}
	wrapped.outputStopped = func() bool { return budget.stopped }
	wrapped.flushOutput = func() { splitter.Flush(answer, inline); flush() }
	originalDiagnostic := callback.onStreamDiagnostic
	wrapped.onStreamDiagnostic = func(diagnostic streamDiagnostic) {
		diagnostic.OutputBudgetLimit = budget.limit
		diagnostic.OutputBudgetUsed = budget.used
		diagnostic.OutputBudgetStopped = budget.stopped
		diagnostic.OutputBudgetTruncated = budget.truncated
		if originalDiagnostic != nil {
			originalDiagnostic(diagnostic)
		}
	}
	return &wrapped
}
