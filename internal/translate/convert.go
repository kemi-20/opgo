package translate

import "encoding/json"

// ConvertRequest 把客户端格式的请求体转换为目标格式的请求体。
func ParseRequest(from Format, raw []byte) (*Request, error) {
	switch from {
	case FormatOpenAICompletions:
		return parseOpenAICompletionsRequest(raw)
	case FormatOpenAIResponses:
		return parseOpenAIResponsesRequest(raw)
	case FormatAnthropic:
		return parseAnthropicRequest(raw)
	default:
		return nil, errf("unsupported request format: %s", from)
	}
}

// RequestOption 可在转换前调整解析出的规范化请求。
type RequestOption func(*Request)

// WithAnthropicMaxTokensFallback 在客户端未提供 max_tokens 时注入模型配置的
// 单次输出上限。仅对目标协议为 Anthropic 的转换生效。
func WithAnthropicMaxTokensFallback(maxTokens int) RequestOption {
	return func(req *Request) {
		if maxTokens > 0 && req.MaxTokens == nil {
			value := maxTokens
			req.MaxTokens = &value
		}
	}
}

func ConvertRequest(from, to Format, raw []byte, opts ...RequestOption) ([]byte, error) {
	if from == to {
		return raw, nil
	}
	req, err := ParseRequest(from, raw)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(req)
	}
	// 计费必需：转换模式下请求标准 usage（上游响应需带 usage 才能按 token 计费）。
	// →completions 流式注入 stream_options.include_usage；Responses 默认在响应对象/
	// response.completed 中返回 usage，且 include:["usage"] 不是合法字段；Anthropic 天然带 usage。
	req.IncludeUsage = true
	switch to {
	case FormatOpenAICompletions:
		return buildOpenAICompletionsRequest(req)
	case FormatOpenAIResponses:
		return buildOpenAIResponsesRequest(req)
	case FormatAnthropic:
		return buildAnthropicRequest(req)
	}
	return raw, nil
}

// ConvertResponse 把目标格式的非流式响应体转换为客户端格式。
func ConvertResponse(to, from Format, raw []byte) ([]byte, error) {
	if to == from {
		return raw, nil
	}
	var resp *Response
	var err error
	switch from {
	case FormatOpenAICompletions:
		resp, err = parseOpenAICompletionsResponse(raw)
	case FormatOpenAIResponses:
		resp, err = parseOpenAIResponsesResponse(raw)
	case FormatAnthropic:
		resp, err = parseAnthropicResponse(raw)
	default:
		return raw, nil
	}
	if err != nil {
		return nil, err
	}
	switch to {
	case FormatOpenAICompletions:
		return buildOpenAICompletionsResponse(resp)
	case FormatOpenAIResponses:
		return buildOpenAIResponsesResponse(resp)
	case FormatAnthropic:
		return buildAnthropicResponse(resp)
	}
	return raw, nil
}

// ConvertResponseMeta 与 ConvertResponse 相同；转换到目标格式响应时提供原始请求的元
// 数据，以便转换后在目标格式上补全字段（temperature/tools/tool_choice 等）。
func ConvertResponseMeta(meta *Request, to, from Format, raw []byte) ([]byte, error) {
	if to == from {
		return raw, nil
	}
	var resp *Response
	var err error
	switch from {
	case FormatOpenAICompletions:
		resp, err = parseOpenAICompletionsResponse(raw)
	case FormatOpenAIResponses:
		resp, err = parseOpenAIResponsesResponse(raw)
	case FormatAnthropic:
		resp, err = parseAnthropicResponse(raw)
	default:
		return raw, nil
	}
	if err != nil {
		return nil, err
	}
	switch to {
	case FormatOpenAICompletions:
		return buildOpenAICompletionsResponse(resp)
	case FormatOpenAIResponses:
		return buildOpenAIResponsesResponseMeta(resp, meta)
	case FormatAnthropic:
		return buildAnthropicResponse(resp)
	}
	return raw, nil
}

// StreamConverter 流式转换器：逐行读源格式 SSE，输出目标格式 SSE 事件。
type StreamConverter struct {
	from, to Format
	reader   *streamReader
	writer   interface {
		Write(StreamEvent, string) [][]byte
	}
	model string
	meta  *Request // 可选：原始客户端请求（用于对齐原生 responses 回显字段）
}

// NewStreamConverter 创建流式转换器。meta 可选：转换到 Responses 时用于回显
// tools/temperature/max_output_tokens 等字段，与原生响应保持一致。
func NewStreamConverter(from, to Format, model string, meta ...*Request) *StreamConverter {
	sc := &StreamConverter{from: from, to: to, reader: newStreamReader(), model: model}
	if len(meta) > 0 {
		sc.meta = meta[0]
	}
	switch to {
	case FormatOpenAICompletions:
		sc.writer = newCompletionsStreamWriter()
	case FormatOpenAIResponses:
		sc.writer = newResponsesStreamWriter(sc.meta)
	case FormatAnthropic:
		sc.writer = newAnthropicStreamWriter()
	}
	return sc
}

// Feed 处理一行 SSE（不含结尾空行），返回目标格式的 SSE 字节（可能为空）。
// 遇到流结束（[DONE]/message_stop）返回 done=true。
func (sc *StreamConverter) Feed(line []byte) ([][]byte, bool) {
	var events []StreamEvent
	switch sc.from {
	case FormatOpenAICompletions:
		events = sc.reader.readCompletionsLine(line)
	case FormatOpenAIResponses:
		events = sc.reader.readResponsesLine(line)
	case FormatAnthropic:
		events = sc.reader.readAnthropicLine(line)
	}
	var out [][]byte
	done := false
	for _, ev := range events {
		if ev.Kind == "done" {
			done = true
		}
		out = append(out, sc.writer.Write(ev, sc.model)...)
	}
	return out, done
}

// Close 在流结束时补齐终止事件（如 [DONE] / message_stop）。
func (sc *StreamConverter) Close() [][]byte {
	if sc.to == FormatOpenAICompletions {
		return [][]byte{[]byte("data: [DONE]\n\n")}
	}
	if sc.to == FormatAnthropic {
		out := sc.writer.Write(StreamEvent{Kind: "done"}, sc.model)
		stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
		return append(out, sseData(stop))
	}
	if sc.to == FormatOpenAIResponses {
		return sc.writer.Write(StreamEvent{Kind: "done"}, sc.model)
	}
	return nil
}

// FormatPath 返回格式对应的上游路径后缀（去掉 /v1 后）。
func FormatPath(f Format) string {
	switch f {
	case FormatOpenAICompletions:
		return "/chat/completions"
	case FormatOpenAIResponses:
		return "/responses"
	case FormatAnthropic:
		return "/messages"
	}
	return ""
}
