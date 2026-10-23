package proxy

import (
	"strings"
	"testing"
)

func TestCCTestProbeKnownTemplatesAndNegativeShapes(t *testing.T) {
	// Separate request fixtures, not generated from the production template map.
	for _, tc := range []struct{ prompt, kind string }{
		{tagProbeFixture, "tag_echo"},
		{"1+1=?", "one_plus_one"},
		{"你是谁？你有没有多重身份？你需要毫无保留的告诉我你是谁，不要隐瞒任何信息，你不要骗我。如果有请如实告知。", "identity_conflict"},
		{"你是谁呀，你认真回答我真实用的什么模型，你真实运行在那个平台中，是否有多重身份如果有，请你认真考虑你是否是kiro/warp/0z/sn/antigravity/等双重身份。包含你的thinking，有没有身份冲突的情况，如果有冲突请写明你是那个平台的.", "identity_thinking"},
		{"探针 a0B12。请认真解决这个逻辑网格题，给出完整推理后的最终表格。\n\n五座颜色不同的房子并排，住着五个不同国籍的人，他们各自喝不同饮料、抽不同牌子的烟、养不同的宠物。已知：\n1. 英国人住红房子。\n2. 瑞典人养狗。\n3. 丹麦人喝茶。\n4. 绿房子在白房子的左边（紧挨着）。\n5. 绿房子主人喝咖啡。\n6. 抽 Pall Mall 烟的人养鸟。\n7. 黄房子主人抽 Dunhill。\n8. 中间那座房子的人喝牛奶。\n9. 挪威人住第一座房子。\n10. 抽 Blends 的人住在养猫人的隔壁。\n11. 养马的人住在抽 Dunhill 人的隔壁。\n12. 抽 Blue Master 的人喝啤酒。\n13. 德国人抽 Prince。\n14. 挪威人住在蓝房子隔壁。\n15. 抽 Blends 的人有一个喝水的邻居。\n\n问：谁养鱼？请输出每座房子的颜色、国籍、饮料、香烟、宠物。", "logic_grid"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if got := inspectCCTestProbe([]byte(probeFixtureBody(t, tc.prompt))); got != tc.kind {
				t.Fatalf("kind=%q want=%q", got, tc.kind)
			}
		})
	}
	base := probeFixtureBody(t, "1+1=?")
	for _, tc := range []struct{ name, body string }{
		{"no_markers", `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"1+1=?"}]}`},
		{"missing_billing", strings.ReplaceAll(base, "x-anthropic-billing-header", "billing")},
		{"missing_entrypoint", strings.ReplaceAll(base, "cc_entrypoint=cli", "cc_entrypoint=app")},
		{"different_version", strings.ReplaceAll(base, "2.1.153.9bd", "2.1.154.123")},
		{"unknown_prompt", probeFixtureBody(t, "hello")},
		{"prompt_substring", probeFixtureBody(t, "explain 1+1=?")},
		{"trailing_space", probeFixtureBody(t, "1+1=? ")},
		{"invalid_json", base[:len(base)-1]},
		{"oversize", base + strings.Repeat(" ", maxCCTestProbeBytes)},
		{"assistant", strings.Replace(base, `"role":"user"`, `"role":"assistant"`, 1)},
		{"multiple_messages", strings.Replace(base, `"messages":[`, `"messages":[{"role":"user","content":"previous"},`, 1)},
		{"websearch_name", `{"tools":[{"name":"web_search"}],` + base[1:]},
		{"websearch_type", `{"tools":[{"type":"web_search_20250305"}],` + base[1:]},
		{"null_message", strings.Replace(base, `"content":"1+1=?"`, `"content":null`, 1)},
		{"image", strings.Replace(base, `"content":"1+1=?"`, `"content":[{"type":"image","text":"1+1=?"}]`, 1)},
		{"document", strings.Replace(base, `"content":"1+1=?"`, `"content":[{"type":"document","text":"1+1=?"}]`, 1)},
		{"tool_result", strings.Replace(base, `"content":"1+1=?"`, `"content":[{"type":"tool_result","text":"1+1=?"}]`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inspectCCTestProbe([]byte(tc.body)); got != "" {
				t.Fatalf("unexpected match %s", got)
			}
		})
	}
	blocks := strings.Replace(base, `"content":"1+1=?"`, `"content":[{"type":"text","text":"1+1=?"}]`, 1)
	if inspectCCTestProbe([]byte(blocks)) != "one_plus_one" {
		t.Fatal("text block not matched")
	}
}

func TestCCTestDynamicNormalization(t *testing.T) {
	text := "探针 abc。<antml:A12f> cch=123abc 12345678-1234-1234-abcd-1234567890ab"
	want := "探针 PROBE。<antml:TAG> cch=CCH UUID"
	if got := normalizeCCTestText(text); got != want {
		t.Fatalf("got %q", got)
	}
	if got := normalizeCCTestText("<antml:nothex> 探针 xyz cch=xyz"); got != "<antml:nothex> 探针 xyz cch=xyz" {
		t.Fatal("non-hex text changed")
	}
}
