package proxy

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func attachmentPNG(t *testing.T, noisy bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	seed := uint32(1)
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			c := color.NRGBA{A: 255}
			if noisy {
				seed = seed*1664525 + 1013904223
				c.R, c.G, c.B = byte(seed), byte(seed>>8), byte(seed>>16)
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func attachmentBlock(kind, media, data string) map[string]interface{} {
	return map[string]interface{}{"type": "document", "source": map[string]interface{}{"type": kind, "media_type": media, "data": data}}
}

func TestAttachmentTextEncodingParity(t *testing.T) {
	text := strings.Repeat("This is the same document. 中文内容。\n", 100)
	want := estimateApproxTokens(text)
	for _, sourceType := range []string{"text", "base64"} {
		data := text
		if sourceType == "base64" {
			data = base64.StdEncoding.EncodeToString([]byte(text))
		}
		if got := estimateClaudeValueTokens(attachmentBlock(sourceType, "text/plain", data)); got != want {
			t.Errorf("%s count=%d want decoded content=%d", sourceType, got, want)
		}
	}
}

func TestAttachmentImageDimensionsNotEntropy(t *testing.T) {
	var counts []int
	for _, noisy := range []bool{false, true} {
		block := map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": attachmentPNG(t, noisy)}}
		counts = append(counts, estimateClaudeValueTokens(block))
	}
	if counts[0] != counts[1] || counts[0] < 1 || counts[0] > 100 {
		t.Fatalf("128x128 image count must depend on dimensions: solid=%d noisy=%d", counts[0], counts[1])
	}
}

func TestAttachmentCountEndpointAndInferenceParity(t *testing.T) {
	h := newStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) { writeKiroTextResponse(t, w, "hello") })
	srv, _ := newRemoteImageServer(t, true)
	inputs := []interface{}{
		[]interface{}{map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": srv.URL + "/img.png"}}},
		[]interface{}{map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": testPNGBase64}}},
	}
	var counts []int
	for _, content := range inputs {
		body := map[string]interface{}{"model": "claude-sonnet-4.5", "max_tokens": 32, "messages": []interface{}{map[string]interface{}{"role": "user", "content": content}}}
		count := localUsageJSONRequest(t, h, "/v1/messages/count_tokens", body, nil)
		var result map[string]int
		if err := json.Unmarshal(count.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, result["input_tokens"])
		usage := runtimeProtocolUsage(t, localUsageJSONRequest(t, h, "/v1/messages", body, nil), false)
		input, _, _, _ := localUsageTotals("/v1/messages", usage)
		if input != result["input_tokens"] {
			t.Errorf("count=%d inference input=%d", result["input_tokens"], input)
		}
	}
	if counts[0] != counts[1] {
		t.Fatalf("URL and inline image counts differ: %v", counts)
	}
}

func TestAttachmentNestedToolResultAndCacheIdentity(t *testing.T) {
	imageBlock := func(noisy bool) interface{} {
		return map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": attachmentPNG(t, noisy)}}
	}
	first, changed := imageBlock(false), imageBlock(true)
	doc := attachmentBlock("text", "text/plain", "document text")
	nested := map[string]interface{}{"type": "tool_result", "tool_use_id": "call", "content": []interface{}{first, doc}}
	want := estimateClaudeValueTokens(first) + estimateApproxTokens("document text")
	if got := estimateClaudeValueTokens(nested); got != want {
		t.Errorf("nested count=%d want=%d", got, want)
	}
	tracker := newPromptCacheTracker(time.Hour)
	profile := func(block interface{}) *promptCacheProfile {
		req := &ClaudeRequest{Model: "claude-sonnet-4.5", Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{block}}}}
		return tracker.BuildClaudeProfile(req, 10000)
	}
	if profile(first).InputFingerprint == profile(changed).InputFingerprint {
		t.Fatal("different image pixels share a cache fingerprint")
	}
}

func TestAttachmentDocumentFormats(t *testing.T) {
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(`<w:document xmlns:w="urn:w"><w:body><w:p><w:r><w:t>hello world</w:t></w:r></w:p></w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	docx := attachmentBlock("base64", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", base64.StdEncoding.EncodeToString(zipped.Bytes()))
	if got := estimateClaudeValueTokens(docx); got < 2 || got > 8 {
		t.Errorf("DOCX text count=%d, expected semantic text", got)
	}
	for _, pages := range []int{1, 3} {
		pdf := "%PDF-1.4\n1 0 obj << /Type /Pages /Count " + string(rune('0'+pages)) + " >> endobj\n"
		block := attachmentBlock("base64", "application/pdf", base64.StdEncoding.EncodeToString([]byte(pdf)))
		got := estimateClaudeValueTokens(block)
		if got != pages*2300 {
			t.Errorf("PDF %d pages=%d want page estimate=%d", pages, got, pages*2300)
		}
	}
}

func TestAttachmentXLSXCountsReferencedCells(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"xl/sharedStrings.xml":     `<sst><si><t>hello world</t></si><si><t>unused text should not count</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>0</v></c><c><v>42</v></c><c t="inlineStr"><is><t>inline text</t></is></c></row></sheetData></worksheet>`,
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	block := attachmentBlock("base64", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", base64.StdEncoding.EncodeToString(buf.Bytes()))
	want := estimateApproxTokens("hello world\nhello world\n42\ninline text")
	if got := estimateClaudeValueTokens(block); got != want {
		t.Fatalf("cell content count=%d want=%d", got, want)
	}
}

func TestAttachmentXLSXRepeatedTextExpansionBound(t *testing.T) {
	parts := map[string][]byte{
		"xl/sharedStrings.xml":     []byte("<sst><si><t>" + strings.Repeat("a", 1<<20) + "</t></si></sst>"),
		"xl/worksheets/sheet1.xml": []byte("<worksheet><sheetData><row>" + strings.Repeat(`<c t="s"><v>0</v></c>`, 40) + "</row></sheetData></worksheet>"),
	}
	if _, ok := spreadsheetDocumentText(parts); ok {
		t.Fatal("repeated shared-string expansion must respect text limit")
	}
}

func TestAttachmentOfficeArchiveEntryLimitBeforeAllocation(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for index := 0; index < 5000; index++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: "empty-" + strconv.Itoa(index), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(1, func() {
		if _, ok := officeDocumentText(buf.Bytes(), "docx"); ok {
			t.Fatal("archive with excessive entries accepted")
		}
	})
	if allocations > 100 {
		t.Fatalf("entry limit applied after allocating central directory: %.0f allocations", allocations)
	}
}

func TestAttachmentWebPDimensionsAndFallback(t *testing.T) {
	data := make([]byte, 30)
	copy(data, "RIFF")
	copy(data[8:], "WEBPVP8X")
	binary.LittleEndian.PutUint32(data[16:], 10)
	data[24], data[27] = 127, 127
	img := &KiroImage{}
	img.Source.Bytes = base64.StdEncoding.EncodeToString(data)
	if got := estimateImageInputTokens(img); got != 22 {
		t.Errorf("WebP dimensions count=%d want=22", got)
	}
	for _, value := range []string{"!invalid!", base64.StdEncoding.EncodeToString([]byte("not an image"))} {
		img.Source.Bytes = value
		if got := estimateImageInputTokens(img); got != 1536 {
			t.Errorf("unreadable image fallback=%d", got)
		}
	}
	unknown := attachmentBlock("base64", "application/octet-stream", base64.StdEncoding.EncodeToString([]byte("opaque")))
	if got := estimateClaudeValueTokens(unknown); got != 1024 {
		t.Errorf("unknown document estimate=%d", got)
	}
}

func TestAttachmentOpenAIAndKiroPayloadParity(t *testing.T) {
	data := attachmentPNG(t, true)
	imageBlock := map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64," + data}}
	doc := attachmentBlock("base64", "text/plain", base64.StdEncoding.EncodeToString([]byte("hello world")))
	content := []interface{}{map[string]interface{}{"type": "text", "text": "question"}, imageBlock, doc}
	want := estimateApproxTokens("question") + 22 + 2
	if got := estimateOpenAIContentTokens(content); got != want {
		t.Errorf("OpenAI mixed content=%d want=%d", got, want)
	}
	message := &KiroUserInputMessage{Content: "question", Images: []KiroImage{*extractImageFromOpenAIPart(imageBlock)}, Documents: []KiroDocument{*kiroDocumentFromBlock(doc)}}
	if got := estimateKiroUserInputTokens(message, true); got != want {
		t.Errorf("Kiro payload mixed content=%d want=%d", got, want)
	}
}

func TestAttachmentPlainTextSourceAndExistingTextCounts(t *testing.T) {
	// "test" is also syntactically valid base64; the declared text source wins.
	block := attachmentBlock("text", "", "test")
	if got := estimateClaudeValueTokens(block); got != estimateApproxTokens("test") {
		t.Errorf("text source misclassified as image: %d", got)
	}
	content := []interface{}{map[string]interface{}{"type": "text", "text": "hel"}, map[string]interface{}{"type": "text", "text": "lo"}}
	if got := estimateOpenAIContentTokens(content); got != estimateApproxTokens("hello") {
		t.Errorf("ordinary joined text count changed: %d", got)
	}
}
