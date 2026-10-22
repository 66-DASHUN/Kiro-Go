package proxy

import (
	"bytes"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// A bounded, read-only observer: it never rewrites bytes sent to the client.
// Missing upstream usage stays unknown; no estimation or simulated cache applies.
type upstreamUsageReader struct {
	io.ReadCloser
	stream                        bool
	pending                       []byte
	discardLine                   bool
	overflow                      bool
	readError                     bool
	seen                          bool
	model                         string
	input, output, creation, read int64
}

func (u *upstreamUsageReader) Read(p []byte) (int, error) {
	n, err := u.ReadCloser.Read(p)
	if n > 0 {
		u.observe(p[:n])
	}
	if err == io.EOF {
		u.finish()
	} else if err != nil {
		u.readError = true
	}
	return n, err
}

func (u *upstreamUsageReader) observe(p []byte) {
	if !u.stream {
		if len(u.pending)+len(p) > 8<<20 {
			u.overflow = true
			u.pending = nil
		}
		if !u.overflow {
			u.pending = append(u.pending, p...)
		}
		return
	}
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		piece := p
		if end >= 0 {
			piece = p[:end]
		}
		if len(u.pending)+len(piece) > 1<<20 {
			u.discardLine = true
			u.pending = nil
		}
		if !u.discardLine {
			u.pending = append(u.pending, piece...)
		}
		if end < 0 {
			return
		}
		if !u.discardLine {
			u.parseSSELine(u.pending)
		}
		u.pending = nil
		u.discardLine = false
		p = p[end+1:]
	}
}

func (u *upstreamUsageReader) finish() {
	if u.stream {
		if !u.discardLine {
			u.parseSSELine(u.pending)
		}
	} else if !u.overflow {
		u.parse(u.pending)
	}
	u.pending = nil
}

func (u *upstreamUsageReader) parseSSELine(line []byte) {
	if bytes.HasPrefix(line, []byte("data:")) {
		u.parse(bytes.TrimSpace(line[5:]))
	}
}

func (u *upstreamUsageReader) parse(raw []byte) {
	if !gjson.ValidBytes(raw) {
		return
	}
	obj := gjson.ParseBytes(raw)
	for _, path := range []string{"model", "message.model", "response.model"} {
		if v := obj.Get(path); v.Type == gjson.String && len(v.Str) < 256 {
			u.model = v.Str
			break
		}
	}
	usage := obj.Get("usage")
	if !usage.IsObject() {
		usage = obj.Get("message.usage")
	}
	if !usage.IsObject() {
		usage = obj.Get("response.usage")
	}
	if !usage.IsObject() {
		return
	}
	set := func(path string, dst *int64) bool {
		v := usage.Get(path)
		if v.Type != gjson.Number || v.Float() < 0 || v.Float() > 1e12 || v.Float() != float64(v.Int()) {
			return false
		}
		*dst = v.Int()
		u.seen = true
		return true
	}
	if !set("input_tokens", &u.input) {
		set("prompt_tokens", &u.input)
	}
	if !set("output_tokens", &u.output) {
		set("completion_tokens", &u.output)
	}
	set("cache_creation_input_tokens", &u.creation)
	set("cache_read_input_tokens", &u.read)
	// OpenAI includes cached tokens within input/prompt tokens. Only Anthropic's
	// separate cache fields are added when computing total metered tokens.
}

func isUpstreamInference(rpath, method string) bool {
	return method == "POST" && (strings.HasSuffix(rpath, "/messages") || strings.HasSuffix(rpath, "/chat/completions") || strings.HasSuffix(rpath, "/responses"))
}
