package proxy

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxAttachmentEstimateBytes = 32 << 20
	unknownImageInputTokens    = 1600
	unknownDocumentInputTokens = 1024
	pdfPageInputTokens         = 2300
)

// These are local content estimates, not native Claude usage. In particular,
// compressed bytes and base64 envelopes are never treated as prompt text.
func estimateAttachmentTokens(block map[string]interface{}) (int, bool) {
	kind, _ := block["type"].(string)
	if !attachmentEstimateWithinLimit(block, 0) {
		switch kind {
		case "image", "image_url", "input_image":
			return unknownImageInputTokens, true
		case "document", "file", "input_file":
			return unknownDocumentInputTokens, true
		}
	}
	switch kind {
	case "image", "image_url", "input_image":
		return estimateImageInputTokens(extractImageFromClaudeBlock(block)), true
	case "document", "file", "input_file":
		if img := extractImageFromOpenAIPart(block); img != nil {
			return estimateImageInputTokens(img), true
		}
		tokens := unknownDocumentInputTokens
		if source, ok := block["source"].(map[string]interface{}); ok && firstString(source["type"]) == "text" {
			tokens = estimateApproxTokens(firstString(source["data"]))
		} else if doc := kiroDocumentFromBlock(block); doc != nil {
			tokens = estimateDocumentInputTokens(doc)
		}
		// Context/title can be visible to the model; transport filename and media
		// metadata are excluded, as they are not the document's text.
		return tokens + estimateApproxTokens(firstString(block["title"])) + estimateApproxTokens(firstString(block["context"])), true
	}
	return 0, false
}

// Existing translation helpers decode base64 while recognizing attachments.
// Bound their input before calling them so counting does not decode it first.
func attachmentEstimateWithinLimit(block map[string]interface{}, depth int) bool {
	if depth > 4 {
		return false
	}
	limit := (maxAttachmentEstimateBytes+2)/3*4 + 1024
	for _, key := range []string{"data", "file_data", "b64_json", "image_base64", "url", "image_url", "title", "context"} {
		if value, ok := block[key].(string); ok && len(value) > limit {
			return false
		}
	}
	if firstString(block["type"]) == "text" && len(firstString(block["data"])) > maxAttachmentEstimateBytes {
		return false
	}
	for _, key := range []string{"source", "file", "image_url"} {
		if nested, ok := block[key].(map[string]interface{}); ok && !attachmentEstimateWithinLimit(nested, depth+1) {
			return false
		}
	}
	return true
}

func decodeAttachmentBytes(encoded string) ([]byte, bool) {
	if len(encoded) > (maxAttachmentEstimateBytes+2)/3*4 {
		return nil, false
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(encoded); err == nil && len(decoded) <= maxAttachmentEstimateBytes {
			return decoded, true
		}
	}
	return nil, false
}

func estimateImageInputTokens(img *KiroImage) int {
	if img == nil {
		return unknownImageInputTokens
	}
	data, ok := decodeAttachmentBytes(img.Source.Bytes)
	if !ok {
		return unknownImageInputTokens
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	w, h := cfg.Width, cfg.Height
	if err != nil {
		w, h = webpImageDimensions(data)
	}
	if w <= 0 || h <= 0 {
		return unknownImageInputTokens
	}
	// Approximate Claude's image preprocessing: no upscaling, <=1568 per
	// edge and <=1.15 megapixels; approximately one token per 750 pixels.
	width, height := float64(w), float64(h)
	scale := math.Min(1, math.Min(1568/math.Max(width, height), math.Sqrt(1150000/(width*height))))
	return max(1, int(math.Ceil(math.Floor(width*scale)*math.Floor(height*scale)/750)))
}

// Read dimensions only, without decompressing potentially large WebP pixels.
func webpImageDimensions(data []byte) (int, int) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0
	}
	for offset := 12; offset+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		if size > len(data)-start {
			return 0, 0
		}
		chunk := data[start : start+size]
		switch string(data[offset : offset+4]) {
		case "VP8X":
			if len(chunk) >= 10 {
				return 1 + int(chunk[4]) + (int(chunk[5]) << 8) + (int(chunk[6]) << 16), 1 + int(chunk[7]) + (int(chunk[8]) << 8) + (int(chunk[9]) << 16)
			}
		case "VP8L":
			if len(chunk) >= 5 && chunk[0] == 0x2f {
				bits := binary.LittleEndian.Uint32(chunk[1:5])
				return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1
			}
		case "VP8 ":
			if len(chunk) >= 10 && bytes.Equal(chunk[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return int(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff)
			}
		}
		offset = start + size + (size & 1)
	}
	return 0, 0
}

func estimateDocumentInputTokens(doc *KiroDocument) int {
	if doc == nil {
		return unknownDocumentInputTokens
	}
	data, ok := decodeAttachmentBytes(doc.Source.Bytes)
	if !ok {
		return unknownDocumentInputTokens
	}
	switch doc.Format {
	case "txt", "csv", "html", "md":
		if text, ok := attachmentText(data); ok {
			return estimateApproxTokens(text)
		}
	case "docx", "xlsx":
		if text, ok := officeDocumentText(data, doc.Format); ok {
			return estimateApproxTokens(text)
		}
	case "pdf":
		// PDF text and images share page processing. Without a full PDF renderer,
		// use a documented midpoint of the typical 1500-3000 tokens/page range.
		// Compressed/encrypted page trees without readable counts assume one page.
		return pdfPageInputTokens * estimatePDFPages(data)
	}
	// Legacy binary DOC/XLS, unsupported/invalid documents and over-limit
	// archives retain an explicit per-document estimate, independent of base64.
	return unknownDocumentInputTokens
}

func attachmentText(data []byte) (string, bool) {
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		if len(data)%2 != 0 {
			return "", false
		}
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		units := make([]uint16, 0, (len(data)-2)/2)
		for i := 2; i < len(data); i += 2 {
			units = append(units, order.Uint16(data[i:i+2]))
		}
		return string(utf16.Decode(units)), true
	}
	if !utf8.Valid(data) {
		return "", false
	}
	return string(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})), true
}

func officeDocumentText(data []byte, format string) (string, bool) {
	if !boundedOfficeDirectory(data) {
		return "", false
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zr.File) > 4096 {
		return "", false
	}
	var expanded int64
	parts := make(map[string][]byte)
	for _, file := range zr.File {
		name := path.Clean(file.Name)
		selected := format == "docx" && (name == "word/document.xml" || name == "word/footnotes.xml" || name == "word/endnotes.xml" || strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer")) && strings.HasSuffix(name, ".xml")
		selected = selected || format == "xlsx" && (name == "xl/sharedStrings.xml" || strings.HasPrefix(name, "xl/worksheets/") && strings.HasSuffix(name, ".xml"))
		if !selected {
			continue
		}
		if file.UncompressedSize64 > uint64(maxAttachmentEstimateBytes-expanded) {
			return "", false
		}
		r, err := file.Open()
		if err != nil {
			return "", false
		}
		raw, readErr := io.ReadAll(io.LimitReader(r, int64(maxAttachmentEstimateBytes)-expanded+1))
		_ = r.Close()
		expanded += int64(len(raw))
		if readErr != nil || expanded > maxAttachmentEstimateBytes {
			return "", false
		}
		parts[name] = raw
	}
	if len(parts) == 0 {
		return "", false
	}
	if format == "xlsx" {
		return spreadsheetDocumentText(parts)
	}
	var text strings.Builder
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := parts[name]
		decoder := xml.NewDecoder(bytes.NewReader(raw))
		parent := ""
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", false
			}
			switch tok := token.(type) {
			case xml.StartElement:
				parent = tok.Name.Local
				if parent == "tab" {
					text.WriteByte('\t')
				}
				if parent == "br" {
					text.WriteByte('\n')
				}
			case xml.CharData:
				if parent == "t" {
					text.Write(tok)
				}
			case xml.EndElement:
				if tok.Name.Local == "p" || tok.Name.Local == "c" || tok.Name.Local == "si" {
					text.WriteByte('\n')
				}
				parent = ""
			}
		}
	}
	return strings.TrimSpace(text.String()), true
}

// Preflight the central directory before archive/zip allocates an object per
// entry. ZIP64, split/prefixed archives and malformed directories use fallback.
// Walking actual headers also prevents a forged small EOCD count bypassing it.
func boundedOfficeDirectory(data []byte) bool {
	for end := len(data) - 22; end >= max(0, len(data)-22-65535); end-- {
		if binary.LittleEndian.Uint32(data[end:end+4]) != 0x06054b50 {
			continue
		}
		record := data[end:]
		if end+22+int(binary.LittleEndian.Uint16(record[20:22])) != len(data) {
			continue
		}
		if binary.LittleEndian.Uint32(record[4:8]) != 0 {
			return false
		}
		entries := int(binary.LittleEndian.Uint16(record[10:12]))
		if entries > 4096 || int(binary.LittleEndian.Uint16(record[8:10])) != entries {
			return false
		}
		size := uint64(binary.LittleEndian.Uint32(record[12:16]))
		offset := uint64(binary.LittleEndian.Uint32(record[16:20]))
		if offset+size != uint64(end) {
			return false
		}
		count := 0
		for offset < uint64(end) {
			if count >= 4096 || uint64(end)-offset < 46 {
				return false
			}
			header := data[int(offset) : int(offset)+46]
			if binary.LittleEndian.Uint32(header[:4]) != 0x02014b50 {
				return false
			}
			length := 46 + uint64(binary.LittleEndian.Uint16(header[28:30])) + uint64(binary.LittleEndian.Uint16(header[30:32])) + uint64(binary.LittleEndian.Uint16(header[32:34]))
			if length > uint64(end)-offset {
				return false
			}
			offset += length
			count++
		}
		return count == entries
	}
	return false
}

type officeRichText struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (text officeRichText) value() string {
	var value strings.Builder
	value.WriteString(text.Text)
	for _, run := range text.Runs {
		value.WriteString(run.Text)
	}
	return value.String()
}

func spreadsheetDocumentText(parts map[string][]byte) (string, bool) {
	var shared []string
	if raw, ok := parts["xl/sharedStrings.xml"]; ok {
		decoder := xml.NewDecoder(bytes.NewReader(raw))
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", false
			}
			if element, ok := token.(xml.StartElement); ok && element.Name.Local == "si" {
				var rich officeRichText
				if err := decoder.DecodeElement(&rich, &element); err != nil {
					return "", false
				}
				shared = append(shared, rich.value())
			}
		}
	}
	names := make([]string, 0, len(parts))
	for name := range parts {
		if strings.HasPrefix(name, "xl/worksheets/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var text strings.Builder
	for _, name := range names {
		decoder := xml.NewDecoder(bytes.NewReader(parts[name]))
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", false
			}
			if element, ok := token.(xml.StartElement); ok && element.Name.Local == "c" {
				var cell struct {
					Type   string         `xml:"t,attr"`
					Value  string         `xml:"v"`
					Inline officeRichText `xml:"is"`
				}
				if err := decoder.DecodeElement(&cell, &element); err != nil {
					return "", false
				}
				value := cell.Value
				switch cell.Type {
				case "s":
					index, err := strconv.Atoi(value)
					if err != nil || index < 0 || index >= len(shared) {
						return "", false
					}
					value = shared[index]
				case "inlineStr":
					value = cell.Inline.value()
				}
				if len(value) >= maxAttachmentEstimateBytes-text.Len() {
					return "", false
				}
				text.WriteString(value)
				text.WriteByte('\n')
			}
		}
	}
	return strings.TrimSpace(text.String()), len(names) > 0
}

var pdfLeafPagePattern = regexp.MustCompile(`/Type\s*/Page\b`)
var pdfPagesDictionaryPattern = regexp.MustCompile(`(?s)<<[^<>]*/Type\s*/Pages\b[^<>]*>>`)
var pdfPageCountPattern = regexp.MustCompile(`/Count\s+(\d+)\b`)

func estimatePDFPages(data []byte) int {
	pages := len(pdfLeafPagePattern.FindAllIndex(data, 10000))
	for _, dictionary := range pdfPagesDictionaryPattern.FindAll(data, 10000) {
		for _, count := range pdfPageCountPattern.FindAllSubmatch(dictionary, 1) {
			if n, err := strconv.Atoi(string(count[1])); err == nil {
				pages = max(pages, min(n, 10000))
			}
		}
	}
	return max(1, min(pages, 10000))
}
