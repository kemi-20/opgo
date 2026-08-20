package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestSanitizeMuseResponsesToolsIsSurgical(t *testing.T) {
	raw := []byte(`{
  "model": "muse-spark-1.2-contributor",
  "input": "find current news",
  "metadata": {"keep_number": 1.2300, "search_content_types": ["untouched"]},
  "tools": [
    {"type":"function","name":"probe","parameters":{"type":"object","properties":{"search_content_types":{"type":"string"}}}},
    {"type":"web_search","external_web_access":false,"search_content_types":["text","image"]},
    {"type":"web_search_preview","search_content_types":["text"]}
  ],
  "tool_choice": "auto"
}`)

	out, changed := sanitizeMuseResponsesTools(museSparkModel, "openai_responses", raw)
	if !changed {
		t.Fatal("应删除 Muse web_search.search_content_types")
	}

	var got struct {
		Metadata map[string]json.RawMessage   `json:"metadata"`
		Tools    []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Tools[1]["search_content_types"]; ok {
		t.Fatal("web_search.search_content_types 仍存在")
	}
	if _, ok := got.Tools[1]["external_web_access"]; !ok {
		t.Fatal("external_web_access 被误删")
	}
	if _, ok := got.Tools[0]["parameters"]; !ok {
		t.Fatal("function 工具被误改")
	}
	if _, ok := got.Tools[2]["search_content_types"]; !ok {
		t.Fatal("web_search_preview 被误改")
	}
	if _, ok := got.Metadata["search_content_types"]; !ok {
		t.Fatal("tools 外同名字段被误改")
	}
	// 除目标属性外，所有原始字节都必须保留，包括缩进、字段顺序和 1.2300。
	want := bytes.Replace(raw, []byte(`,"search_content_types":["text","image"]`), nil, 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("目标属性以外的字节发生变化\n got: %s\nwant: %s", out, want)
	}
}

func TestSanitizeMuseResponsesToolsDoesNotTouchOtherTraffic(t *testing.T) {
	raw := []byte(`{"model":"x","tools":[{"type":"web_search","search_content_types":["text"]}]}`)
	tests := []struct {
		name   string
		model  string
		format string
	}{
		{"other model", "gpt-5.6-luna", "openai_responses"},
		{"other wire format", museSparkModel, "openai_completions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := sanitizeMuseResponsesTools(tc.model, tc.format, raw)
			if changed || string(out) != string(raw) {
				t.Fatalf("不应修改：changed=%v out=%s", changed, out)
			}
		})
	}
}

func TestSanitizeMuseResponsesToolsNoTargetFieldIsByteExact(t *testing.T) {
	raw := []byte(` { "model":"muse-spark-1.2-contributor", "tools" : [ { "type" : "web_search", "external_web_access" : false } ] } `)
	out, changed := sanitizeMuseResponsesTools(museSparkModel, "openai_responses", raw)
	if changed || string(out) != string(raw) {
		t.Fatalf("无目标字段时必须逐字节不变：changed=%v out=%q", changed, out)
	}
}

func TestMuseFieldRemovalHandlesEveryObjectPosition(t *testing.T) {
	tests := []string{
		`{"search_content_types":["text"],"type":"web_search","x":1}`,
		`{"type":"web_search","search_content_types":["text"],"x":1}`,
		`{"type":"web_search","x":1,"search_content_types":["text"]}`,
		`{"type":"web_search","search_content_types":["text"],"search_content_types":["image"]}`,
	}
	for _, tool := range tests {
		raw := []byte(`{"model":"muse-spark-1.2-contributor","tools":[` + tool + `]}`)
		out, changed := sanitizeMuseResponsesTools(museSparkModel, "openai_responses", raw)
		if !changed || !json.Valid(out) {
			t.Fatalf("未正确清理 %s：changed=%v out=%s", tool, changed, out)
		}
		var envelope struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(out, &envelope); err != nil {
			t.Fatal(err)
		}
		if _, exists := envelope.Tools[0]["search_content_types"]; exists {
			t.Fatalf("仍残留目标字段：%s", out)
		}
	}
}

func TestMuseFieldRemovalHandlesUnicodeEscapedKeys(t *testing.T) {
	// JSON 的 Unicode 转义在源字节中不包含实际双引号；扫描器应完整越过，
	// json.Unmarshal key 后仍能精准识别 tools/type/目标字段。
	raw := []byte(`{"t\u006f\u006fls":[{"t\u0079pe":"web_search","search\u005fcontent_types":["text"],"keep":"x"}]}`)
	out, changed := sanitizeMuseResponsesTools(museSparkModel, "openai_responses", raw)
	if !changed || !json.Valid(out) {
		t.Fatalf("Unicode 转义键未正确处理: changed=%v out=%s", changed, out)
	}
	var envelope struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, exists := envelope.Tools[0]["search_content_types"]; exists {
		t.Fatalf("转义形式目标字段仍残留: %s", out)
	}
	if _, exists := envelope.Tools[0]["keep"]; !exists {
		t.Fatalf("无关字段被误删: %s", out)
	}
}

func TestForwardAppliesMuseCompatibilityAndOnlyToMuse(t *testing.T) {
	upstream := &fakeUpstream{}
	upstreamServer := newFakeServer(upstream)
	defer upstreamServer.Close()
	p, _ := newTestProxy(t, upstreamServer.URL, &fixedBalance{snap: okSnapshot()}, nil)
	server := httptest.NewServer(p)
	defer server.Close()

	museBody := `{"model":"muse-spark-1.2-contributor","input":"search","tools":[{"type":"web_search","external_web_access":false,"search_content_types":["text","image"]}],"tool_choice":"auto"}`
	status, responseBody, _ := doReq(t, server, "POST", "/v1/responses", testUser1, museBody)
	if status != 200 {
		t.Fatalf("Muse status=%d body=%s", status, responseBody)
	}
	upstream.mu.Lock()
	forwardedMuse := append([]byte(nil), upstream.lastBody...)
	forwardedPath := upstream.lastPath
	upstream.mu.Unlock()
	if forwardedPath != "/responses" {
		t.Fatalf("Muse upstream path=%q, want /responses", forwardedPath)
	}
	var museRequest struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(forwardedMuse, &museRequest); err != nil {
		t.Fatal(err)
	}
	if _, exists := museRequest.Tools[0]["search_content_types"]; exists {
		t.Fatalf("代理链路未删除目标字段：%s", forwardedMuse)
	}
	if _, exists := museRequest.Tools[0]["external_web_access"]; !exists {
		t.Fatalf("代理链路误删有效字段：%s", forwardedMuse)
	}

	otherBody := `{"model":"gpt-5.6-luna","input":"search","tools":[{"type":"web_search","external_web_access":false,"search_content_types":["text","image"]}],"tool_choice":"auto"}`
	status, responseBody, _ = doReq(t, server, "POST", "/v1/responses", testUser1, otherBody)
	if status != 200 {
		t.Fatalf("其他模型 status=%d body=%s", status, responseBody)
	}
	upstream.mu.Lock()
	forwardedOther := string(upstream.lastBody)
	upstream.mu.Unlock()
	if forwardedOther != otherBody {
		t.Fatalf("其他模型请求必须逐字节不变\n got: %s\nwant: %s", forwardedOther, otherBody)
	}
}
