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
		"muse-spark-1.2-contributor",
		"minimax-m3",
		"longcat-2.0",
		"x-preview-f-free",
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

	v := cfg.Pricing["deepseek-v4-flash"]
	if v.ProviderModel != "deepseek-v4-flash" || v.ContextLength != 1000000 || v.MaxOutputTokens != 384000 ||
		v.Transformation != "" || v.Modality != "text->text" || v.Tag == "" || v.Peak == nil {
		t.Fatalf("first example model=%+v, should demonstrate every field", v)
	}

	v = cfg.Pricing["deepseek-v4-flash-vision-exp"]
	if v.InputPerMillion != 0.88 || v.OutputPerMillion != 2.64 || v.CachedReadPerMillion != 0.028 {
		t.Fatalf("vision price=%+v", v)
	}
	if v.ContextLength != 0 || v.MaxOutputTokens != 0 || v.Transformation != "" || v.Modality != "text+image->text" || v.Peak == nil {
		t.Fatalf("vision metadata=%+v", v)
	}

	v = cfg.Pricing["deepseek-v4-pro"]
	if v.InputPerMillion != 2.64 || v.OutputPerMillion != 7.92 || v.CachedReadPerMillion != 0.088 {
		t.Fatalf("pro price=%+v, want官网价的4倍", v)
	}

	v = cfg.Pricing["longcat-2.0"]
	if v.InputPerMillion != 0.30 || v.OutputPerMillion != 1.20 || v.CachedReadPerMillion != 0.006 {
		t.Fatalf("LongCat price=%+v", v)
	}
	if v.Transformation != "openai_completions" || v.Modality != "text->text" {
		t.Fatalf("LongCat metadata=%+v", v)
	}

	v = cfg.Pricing["minimax-m3"]
	if v.InputPerMillion != 0.30 || v.OutputPerMillion != 1.20 || v.CachedReadPerMillion != 0.06 {
		t.Fatalf("MiniMax M3 price=%+v", v)
	}
	if v.Transformation != "" || v.Modality != "text+image+video->text" {
		t.Fatalf("MiniMax M3 metadata=%+v", v)
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
