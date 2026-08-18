package main

import (
	"os"
	"testing"

	"opgo/internal/config"
)

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
}
