package meter

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"opgo/internal/config"
)

// UnitsPerUSD 1 美元 = 1e8 微元（8 位小数精度）。
const UnitsPerUSD = 1e8

// Usage 一次请求的 token 用量。
type Usage struct {
	PromptTokens      int64
	CompletionTokens  int64
	TotalTokens       int64
	CachedTokens      int64 // 缓存读取（OpenAI cached_tokens / Anthropic cache_read）
	CachedWriteTokens int64 // 缓存写入（Anthropic cache_creation）
}

// USDToUnits 美元金额转整数微元。
func USDToUnits(v float64) int64 { return int64(math.Round(v * UnitsPerUSD)) }

// UnitsToUSD 微元转美元。
func UnitsToUSD(u int64) float64 { return float64(u) / UnitsPerUSD }

// CostUnits 按模型价格计算费用（微元）。
func CostUnits(p config.ModelPricing, u Usage) int64 {
	prompt := u.PromptTokens - u.CachedTokens - u.CachedWriteTokens
	if prompt < 0 {
		prompt = 0
	}
	cost := float64(prompt)/1e6*p.InputPerMillion +
		float64(u.CachedTokens)/1e6*p.CachedReadPerMillion +
		float64(u.CachedWriteTokens)/1e6*p.CachedWritePerMillion +
		float64(u.CompletionTokens)/1e6*p.OutputPerMillion
	return int64(math.Round(cost * UnitsPerUSD))
}

// RequestModel 从请求体提取模型名。
func RequestModel(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	return obj.Model
}

// EnsureStreamUsage 对流式 OpenAI 风格请求注入 stream_options.include_usage=true（若缺失）。
// 采用字节级最小注入：仅在 JSON 对象开头插入一个键，其余字节（图片 base64、字段顺序、
// 数字精度）完全不动；仅当请求已带 stream_options 但缺 include_usage 时才整体重排（罕见）。
func EnsureStreamUsage(body []byte) ([]byte, bool) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	stream, _ := obj["stream"].(bool)
	if !stream {
		return body, false
	}
	if so, _ := obj["stream_options"].(map[string]any); so != nil {
		if inc, _ := so["include_usage"].(bool); inc {
			return body, false
		}
		so["include_usage"] = true
		out, err := json.Marshal(obj)
		if err != nil {
			return body, false
		}
		return out, true
	}
	trim := bytes.TrimLeft(body, " \t\r\n")
	if len(trim) == 0 || trim[0] != '{' {
		return body, false
	}
	off := len(body) - len(trim)
	inject := []byte(`"stream_options":{"include_usage":true},`)
	out := make([]byte, 0, len(body)+len(inject)+1)
	out = append(out, body[:off+1]...)
	out = append(out, inject...)
	out = append(out, body[off+1:]...)
	return out, true
}
// usageJSON 同时兼容 OpenAI 与 Anthropic 的用量字段。
type usageJSON struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheRead           int64 `json:"cache_read_input_tokens"`
	CacheCreation       int64 `json:"cache_creation_input_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *usageJSON) usage() (Usage, bool) {
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		// OpenAI 风格：prompt_tokens / completion_tokens
		us := Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
		if u.PromptTokensDetails != nil {
			us.CachedTokens = u.PromptTokensDetails.CachedTokens
		}
		return us, true
	}
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		// Anthropic / Responses 风格：input_tokens / output_tokens
		cached := u.CacheRead
		if cached == 0 && u.InputTokensDetails != nil {
			cached = u.InputTokensDetails.CachedTokens
		}
		return Usage{
			PromptTokens:      u.InputTokens,
			CompletionTokens:  u.OutputTokens,
			TotalTokens:       u.InputTokens + u.OutputTokens,
			CachedTokens:      cached,
			CachedWriteTokens: u.CacheCreation,
		}, true
	}
	if u.TotalTokens > 0 {
		// 只有总数时兜底
		return Usage{TotalTokens: u.TotalTokens}, true
	}
	return Usage{}, false
}

// ParseBodyUsage 从非流式 JSON 响应提取用量（OpenAI 与 Anthropic 格式）。
func ParseBodyUsage(body []byte) (Usage, bool) {
	var obj struct {
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Usage == nil {
		return Usage{}, false
	}
	return obj.Usage.usage()
}

// ParseSSEUsage 解析一行 SSE 事件，支持：
// OpenAI chat 流末尾 usage chunk、Anthropic message_start/message_delta、Responses response.completed。
func ParseSSEUsage(line []byte) (Usage, bool) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return Usage{}, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return Usage{}, false
	}
	var obj struct {
		Type    string     `json:"type"`
		Usage   *usageJSON `json:"usage"`
		Message *struct {
			Usage *usageJSON `json:"usage"`
		} `json:"message"`
		Response *struct {
			Usage *usageJSON `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return Usage{}, false
	}
	switch obj.Type {
	case "":
		if obj.Usage != nil {
			return obj.Usage.usage()
		}
		return Usage{}, false
	case "message_start":
		if obj.Message != nil && obj.Message.Usage != nil {
			return obj.Message.Usage.usage()
		}
	case "message_delta":
		if obj.Usage != nil {
			u, _ := obj.Usage.usage()
			return Usage{CompletionTokens: u.CompletionTokens}, true
		}
	case "response.completed", "response.incomplete":
		if obj.Response != nil && obj.Response.Usage != nil {
			return obj.Response.Usage.usage()
		}
	}
	return Usage{}, false
}
