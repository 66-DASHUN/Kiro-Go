package kiro

import (
	"strings"
	"unicode"
)

// StopSequenceFilter exposes the reference stream's pending-prefix algorithm
// without duplicating its matching or tie-breaking helpers.
type StopSequenceFilter struct {
	Sequences []string
	pending   string
	matched   string
}

func (s *StopSequenceFilter) Push(text string) string {
	if s.matched != "" || text == "" {
		return ""
	}
	s.pending += text
	if index, matched := firstStopSequenceIndex(s.pending, s.Sequences); matched != "" {
		text = s.pending[:index]
		s.pending = ""
		s.matched = matched
		return text
	}
	suffix := stopSequencePotentialSuffix(s.pending, s.Sequences)
	text = s.pending[:len(s.pending)-len(suffix)]
	s.pending = suffix
	return text
}

func (s *StopSequenceFilter) Flush() string {
	text := s.pending
	s.pending = ""
	return text
}

// ThinkingTextSplitter follows processThinkingTaggedText/flushThinkingAtBoundary
// in the pinned translator, using the same tag-boundary and safe-flush helpers.
// It is only used when a stop filter must distinguish answer text from inline
// thinking before invoking the existing transport callbacks.
type ThinkingTextSplitter struct {
	buffer              string
	inThinking          bool
	stripLeadingNewline bool
}

func (s *ThinkingTextSplitter) Push(text string, emit func(string, bool)) {
	s.buffer += text
	for {
		if !s.inThinking {
			if start := findRealThinkingStartTag(s.buffer, 0); start != -1 {
				if before := s.buffer[:start]; strings.TrimSpace(before) != "" {
					emit(before, false)
				}
				s.inThinking = true
				s.stripLeadingNewline = true
				s.buffer = s.buffer[start+len(thinkingStartTag):]
				continue
			}
			if n := safeThinkingStreamFlushLen(s.buffer, len(thinkingStartTag)); n > 0 && strings.TrimSpace(s.buffer[:n]) != "" {
				emit(s.buffer[:n], false)
				s.buffer = s.buffer[n:]
			}
			return
		}
		if s.stripLeadingNewline {
			if strings.HasPrefix(s.buffer, "\n") {
				s.buffer = s.buffer[1:]
				s.stripLeadingNewline = false
			} else if s.buffer != "" {
				s.stripLeadingNewline = false
			}
		}
		if end := findStreamThinkingEndTagStrict(s.buffer, 0); end != -1 {
			if end > 0 {
				emit(s.buffer[:end], true)
			}
			s.inThinking = false
			s.buffer = s.buffer[end+len(thinkingEndTag)+len("\n\n"):]
			continue
		}
		if n := safeThinkingStreamFlushLen(s.buffer, len(thinkingEndTag)+len("\n\n")); n > 0 {
			emit(s.buffer[:n], true)
			s.buffer = s.buffer[n:]
		}
		return
	}
}

func (s *ThinkingTextSplitter) Flush(emit func(string, bool)) {
	if s.buffer == "" {
		return
	}
	if s.inThinking {
		if end := findStreamThinkingEndTagAtBufferEnd(s.buffer, 0); end != -1 {
			if end > 0 {
				emit(s.buffer[:end], true)
			}
			remaining := strings.TrimLeftFunc(s.buffer[end+len(thinkingEndTag):], unicode.IsSpace)
			s.buffer = ""
			s.inThinking = false
			if remaining != "" {
				emit(remaining, false)
			}
			return
		}
		emit(s.buffer, true)
		s.buffer = ""
		s.inThinking = false
		return
	}
	text := s.buffer
	s.buffer = ""
	emit(text, false)
}
