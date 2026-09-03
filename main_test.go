package main

import (
	"os"
	"strings"
	"testing"

	"opgo/internal/config"
)

func TestExampleModelCatalog(t *testing.T) {
	cfg, err := config.Parse(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"deepseek-v4-flash",
		"deepseek-v4-flash-vision-exp",
		"deepseek-v4-pro",
		"mimo-v2.5",
		"gpt-5.6-luna",
		"hy3",
		"muse-spark-1.3-contributor",
		"minimax-m3",
		"longcat-2.0",
		"glm-5.3-flash",
	}
	got := cfg.ModelNames()
	if len(got) != len(wantOrder) {
		t.Fatalf("model count=%d, want %d: %v", len(got), len(wantOrder), got)
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Fatalf("model order=%v, want %v", got, wantOrder)
		}
	}

	// 第一个模型仍是完整示例（演示所有可选字段）
	v := cfg.Pricing["deepseek-v4-flash"]
	if v.ProviderModel != "deepseek-v4-flash" || v.ContextLength != 1000000 || v.MaxOutputTokens != 384000 ||
		v.Transformation != "" || v.Modality != "text->text" || v.Tag == "" || v.Peak == nil {
		t.Fatalf("first example model=%+v, should demonstrate every field", v)
	}

	v = cfg.Pricing["deepseek-v4-flash-vision-exp"]
	if v.InputPerMillion != 0.88 || v.OutputPerMillion != 2.64 || v.CachedReadPerMillion != 0.028 {
		t.Fatalf("vision price=%+v, want 官网 4x（$15 档 谷时）", v)
	}
	if v.ContextLength != 1000000 || v.MaxOutputTokens != 384000 ||
		v.Transformation != "" || v.Modality != "text+image->text" || v.Peak == nil {
		t.Fatalf("vision metadata=%+v", v)
	}

	v = cfg.Pricing["deepseek-v4-pro"]
	if v.InputPerMillion != 2.64 || v.OutputPerMillion != 7.92 || v.CachedReadPerMillion != 0.088 {
		t.Fatalf("pro price=%+v, want 官网 4x（$15 档 谷时）", v)
	}
	if v.ContextLength != 1000000 || v.MaxOutputTokens != 384000 || v.Peak == nil {
		t.Fatalf("pro metadata=%+v", v)
	}

	v = cfg.Pricing["mimo-v2.5"]
	if v.InputPerMillion != 0.14 || v.OutputPerMillion != 0.28 || v.CachedReadPerMillion != 0.0028 {
		t.Fatalf("mimo price=%+v", v)
	}
	if v.ContextLength != 1000000 || v.MaxOutputTokens != 128000 ||
		v.Transformation != "openai_completions" || v.Modality != "text+image+audio+video->text" {
		t.Fatalf("mimo metadata=%+v", v)
	}

	v = cfg.Pricing["gpt-5.6-luna"]
	if v.InputPerMillion != 1.60 || v.OutputPerMillion != 7.20 ||
		v.CachedReadPerMillion != 0.16 || v.CachedWritePerMillion != 2.00 {
		t.Fatalf("GPT-5.6-Luna price=%+v, want 官网 4x×>272K 翻倍（$15 档）", v)
	}
	if v.ContextLength != 1050000 || v.MaxOutputTokens != 128000 ||
		v.Transformation != "openai_responses" || v.Modality != "text+image->text" {
		t.Fatalf("GPT-5.6-Luna metadata=%+v", v)
	}
	if v.Peak != nil || v.Tag != "" {
		t.Fatalf("GPT-5.6-Luna 应无 peak/tag=%+v", v)
	}

	v = cfg.Pricing["hy3"]
	if v.InputPerMillion != 0.14 || v.OutputPerMillion != 0.58 || v.CachedReadPerMillion != 0.035 {
		t.Fatalf("hy3 price=%+v", v)
	}
	if v.ContextLength != 256000 || v.MaxOutputTokens != 64000 ||
		v.Transformation != "openai_completions" || v.Modality != "text->text" {
		t.Fatalf("hy3 metadata=%+v", v)
	}

	v = cfg.Pricing["muse-spark-1.3-contributor"]
	if v.InputPerMillion != 0.10 || v.OutputPerMillion != 0.20 || v.CachedReadPerMillion != 0.002 {
		t.Fatalf("muse price=%+v", v)
	}
	if v.ContextLength != 1048576 || v.MaxOutputTokens != 131072 ||
		v.Transformation != "openai_responses" || v.Modality != "text+image+audio->text" {
		t.Fatalf("muse metadata=%+v", v)
	}

	v = cfg.Pricing["minimax-m3"]
	if v.InputPerMillion != 0.30 || v.OutputPerMillion != 1.20 || v.CachedReadPerMillion != 0.06 {
		t.Fatalf("MiniMax M3 price=%+v", v)
	}
	if v.ContextLength != 1000000 || v.MaxOutputTokens != 131072 ||
		v.Transformation != "" || v.Modality != "text+image+video->text" {
		t.Fatalf("MiniMax M3 metadata=%+v", v)
	}

	v = cfg.Pricing["longcat-2.0"]
	if v.InputPerMillion != 0.30 || v.OutputPerMillion != 1.20 || v.CachedReadPerMillion != 0.006 {
		t.Fatalf("LongCat price=%+v", v)
	}
	if v.ContextLength != 1000000 || v.MaxOutputTokens != 131072 ||
		v.Transformation != "openai_completions" || v.Modality != "text->text" {
		t.Fatalf("LongCat metadata=%+v", v)
	}

	v = cfg.Pricing["glm-5.3-flash"]
	if v.InputPerMillion != 0.30 || v.OutputPerMillion != 1.00 || v.CachedReadPerMillion != 0.06 {
		t.Fatalf("GLM-5.3-Flash price=%+v, want 官网 4x（$15 档）", v)
	}
	if v.ContextLength != 1000000 || v.MaxOutputTokens != 131072 ||
		v.Transformation != "openai_completions" || v.Modality != "text+image->text" {
		t.Fatalf("GLM-5.3-Flash metadata=%+v", v)
	}
	if v.Peak != nil || v.Tag != "" {
		t.Fatalf("GLM-5.3-Flash 应无 peak/tag=%+v", v)
	}
}

func TestAssetsMatchSource(t *testing.T) {
	ex, err := os.ReadFile("config.example.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if string(exampleConfig) != string(ex) {
		t.Error("exampleConfig 与 config.example.jsonc 不一致，请重新运行 scripts/gen_assets.ps1")
	}
	html, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(indexHTML) != string(html) {
		t.Error("indexHTML 与 web/index.html 不一致，请重新运行 scripts/gen_assets.ps1")
	}
	if _, err := config.Parse(exampleConfig); err != nil {
		t.Errorf("示例配置校验失败: %v", err)
	}
	if strings.Contains(strings.ToLower(string(exampleConfig)), "opencode") {
		t.Error("内嵌示例配置不应包含供应商字样")
	}
}
