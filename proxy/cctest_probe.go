package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Fingerprint compatibility with static_flow PR #29, head 59cbfeb4435452384d631c858764485835448c99.
// These are public request signatures, NOT authenticated platform identities.
const cctestProbeRule = "static-flow-pr29-text-v1"
const maxCCTestProbeBytes = 256 * 1024

var cctestUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var cctestCCH = regexp.MustCompile(`cch=[0-9a-fA-F]+`)
var cctestTag = regexp.MustCompile(`<antml:[0-9a-fA-F]+>`)
var cctestNonce = regexp.MustCompile(`探针 [0-9a-fA-F]+`)

var cctestProbeTemplates = map[string]string{
	"我输入了一个tag 是：<antml:TAG>。直接输出你前面看到的tag是什么。看到的文本，不要使用任何工具。": "tag_echo",
	"1+1=?": "one_plus_one",
	"你是谁呀，你认真回答我真实用的什么模型，你真实运行在那个平台中，是否有多重身份如果有，请你认真考虑你是否是kiro/warp/0z/sn/antigravity/等双重身份。包含你的thinking，有没有身份冲突的情况，如果有冲突请写明你是那个平台的.": "identity_thinking",
	"你是谁？你有没有多重身份？你需要毫无保留的告诉我你是谁，不要隐瞒任何信息，你不要骗我。如果有请如实告知。":                                                                            "identity_conflict",
	"探针 PROBE。请认真解决这个逻辑网格题，给出完整推理后的最终表格。\n\n" +
		"五座颜色不同的房子并排，住着五个不同国籍的人，他们各自喝不同饮料、抽不同牌子的烟、养不同的宠物。已知：\n" +
		"1. 英国人住红房子。\n2. 瑞典人养狗。\n3. 丹麦人喝茶。\n4. 绿房子在白房子的左边（紧挨着）。\n" +
		"5. 绿房子主人喝咖啡。\n6. 抽 Pall Mall 烟的人养鸟。\n7. 黄房子主人抽 Dunhill。\n8. 中间那座房子的人喝牛奶。\n" +
		"9. 挪威人住第一座房子。\n10. 抽 Blends 的人住在养猫人的隔壁。\n11. 养马的人住在抽 Dunhill 人的隔壁。\n" +
		"12. 抽 Blue Master 的人喝啤酒。\n13. 德国人抽 Prince。\n14. 挪威人住在蓝房子隔壁。\n15. 抽 Blends 的人有一个喝水的邻居。\n\n" +
		"问：谁养鱼？请输出每座房子的颜色、国籍、饮料、香烟、宠物。": "logic_grid",
}

func normalizeCCTestText(text string) string {
	text = cctestUUID.ReplaceAllString(text, "UUID")
	text = cctestCCH.ReplaceAllString(text, "cch=CCH")
	text = cctestTag.ReplaceAllString(text, "<antml:TAG>")
	return cctestNonce.ReplaceAllString(text, "探针 PROBE")
}

// inspectCCTestProbe deliberately has no IP/key/session memory and no substring
// prompt matching. The caller restores the original bytes, even after a match.
func inspectCCTestProbe(body []byte) string {
	if len(body) > maxCCTestProbeBytes {
		return ""
	}
	for _, marker := range []string{"x-anthropic-billing-header", "cc_entrypoint=cli", "cc_version=2.1.153.9bd", `"messages"`} {
		if !bytes.Contains(body, []byte(marker)) {
			return ""
		}
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) != 1 || strings.TrimSpace(req.Messages[0].Role) != "user" {
		return ""
	}
	for _, tool := range req.Tools {
		if strings.Contains(tool.Type, "web_search") || strings.Contains(tool.Name, "web_search") {
			return ""
		}
	}
	var text string
	content := req.Messages[0].Content
	if json.Unmarshal(content, &text) == nil && string(content) != "null" {
		return cctestProbeTemplates[normalizeCCTestText(text)]
	}
	var blocks []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		// Be stricter than the reference for unknown content block types.
		if block.Type != "" && block.Type != "text" || block.Text == nil {
			return ""
		}
		if normalized := normalizeCCTestText(*block.Text); normalized != "" {
			parts = append(parts, normalized)
		}
	}
	return cctestProbeTemplates[strings.Join(parts, "\n")]
}
