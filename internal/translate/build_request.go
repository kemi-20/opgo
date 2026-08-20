package translate

import (
	"encoding/json"
	"strings"
)

// ---------- 序列化到 OpenAI chat/completions ----------

func buildOpenAICompletionsRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		if len(req.Stop) == 1 {
			out["stop"] = req.Stop[0]
		} else {
			out["stop"] = req.Stop
		}
	}
	if req.ReasoningEffort != "" {
		out["reasoning_effort"] = normalizeCompletionsReasoningEffort(req.Model, req.ReasoningEffort)
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	// tool_choice：Responses 的 {"type":"function","name":"X"} 转 completions 的
	// {"type":"function","function":{"name":"X"}}；字符串与 built-in 原样保留。
	if rawTC, ok := req.ToolChoice.(json.RawMessage); ok && len(rawTC) > 0 {
		if tc := convertResponsesToolChoiceToCompletions(rawTC); tc != nil {
			out["tool_choice"] = tc
		}
	}
	// messages
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if len(req.System) > 0 {
		// 多个 system/instructions 块用换行分隔（避免 "A.B." 粘连）
		msgs = append(msgs, map[string]any{"role": "system", "content": joinSystemBlocks(req.System)})
	}
	// Chat Completions 的 role=tool 只允许文本。工具结果中的图片/音频先累积，
	// 等连续的 tool 消息全部写完后，再用紧随其后的 user 多模态消息交给模型。
	var pendingToolMedia []Block
	flushToolMedia := func() {
		if len(pendingToolMedia) == 0 {
			return
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": blocksToOpenAIContent(pendingToolMedia)})
		pendingToolMedia = nil
	}
	appendToolResult := func(callID string, blocks []Block, fallback string) {
		text, media := splitToolContentForCompletions(blocks, fallback)
		if text == "" && len(media) > 0 {
			text = "Tool returned media attachments; see the following user message."
		}
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": text})
		if len(media) > 0 {
			pendingToolMedia = append(pendingToolMedia, toolMediaFollowupBlocks(callID, media)...)
		}
	}
	for _, m := range req.Messages {
		toolCarrier := m.Role == "tool" || (m.Role == "user" && hasToolResultBlock(m.Content))
		if !toolCarrier {
			if m.Role == "user" && len(pendingToolMedia) > 0 {
				// 若工具结果后正好还有普通 user 内容，把附件并入该消息，避免产生
				// 两条连续 user 消息；工具的 role=tool 文本仍已先行写入。
				combined := append([]Block(nil), pendingToolMedia...)
				pendingToolMedia = nil
				if len(m.Content) > 0 {
					combined = append(combined, m.Content...)
				} else if m.Text != "" {
					combined = append(combined, Block{Type: "text", Text: m.Text})
				}
				m.Content = combined
				m.Text = ""
			} else {
				flushToolMedia()
			}
		}
		mm := map[string]any{"role": m.Role}
		switch m.Role {
		case "tool":
			appendToolResult(m.ToolCallID, m.Content, m.Text)
			continue
		case "assistant":
			var textParts []Block
			var thinking string
			var toolCalls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "thinking":
					thinking += b.Thinking
				case "tool_use":
					var args string
					if len(b.Input) > 0 {
						args = string(b.Input)
					} else {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":       b.ToolUseID,
						"type":     "function",
						"function": map[string]any{"name": b.Name, "arguments": args},
					})
				default:
					textParts = append(textParts, b)
				}
			}
			if len(m.Content) == 0 && m.Text != "" {
				textParts = append([]Block{{Type: "text", Text: m.Text}}, textParts...)
			}
			if len(textParts) > 0 {
				mm["content"] = blocksToOpenAIContent(textParts)
			} else {
				mm["content"] = ""
			}
			if thinking != "" {
				mm["reasoning_content"] = thinking
			}
			if len(toolCalls) > 0 {
				mm["tool_calls"] = toolCalls
			}
		case "user":
			// OpenAI 历史中工具结果（function_call_output / tool_result 块）必须转为独立的
			// role=tool 消息，否则上游模型认为工具未执行。
			if len(m.Content) > 0 && hasToolResultBlock(m.Content) {
				for _, b := range m.Content {
					if b.Type == "tool_result" {
						appendToolResult(b.ToolUseID, toolResultBlocks(b.Content), toolResultText(b.Content))
					}
				}
				// 同一 user 消息中除工具结果外的普通文本（如 "结果拿到了，现在做 X"）
				// 必须保留为独立 user 消息，否则多轮对话文本丢失、模型无法继续。
				var extra []Block
				for _, b := range m.Content {
					if b.Type != "tool_result" {
						extra = append(extra, b)
					}
				}
				if len(extra) > 0 {
					// 同一轮的附件与普通 user 内容合并，避免连续 user 消息，同时保证
					// 所有 tool 结果仍排在附件之前。
					combined := append(append([]Block(nil), pendingToolMedia...), extra...)
					pendingToolMedia = nil
					msgs = append(msgs, map[string]any{"role": "user", "content": blocksToOpenAIContent(combined)})
				}
				continue
			} else if len(m.Content) > 0 {
				mm["content"] = blocksToOpenAIContent(m.Content)
			} else {
				mm["content"] = m.Text
			}
		default:
			if len(m.Content) > 0 {
				mm["content"] = blocksToOpenAIContent(m.Content)
			} else {
				mm["content"] = m.Text
			}
		}
		msgs = append(msgs, mm)
	}
	flushToolMedia()
	out["messages"] = msgs
	// tools
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type == "function" {
				fn := map[string]any{"name": t.Name}
				if t.Description != "" {
					fn["description"] = t.Description
				}
				if len(t.Parameters) > 0 {
					fn["parameters"] = json.RawMessage(t.Parameters)
				}
				tools = append(tools, map[string]any{"type": "function", "function": fn})
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if req.Stream && req.IncludeUsage {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

// normalizeCompletionsReasoningEffort 处理上游模型的枚举差异。
// hy3 仅接受 no_think/low/high，而 Codex 可发送 medium/xhigh/max/none/minimal。
func normalizeCompletionsReasoningEffort(model, effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if model != "hy3" {
		return effort
	}
	switch effort {
	case "none", "minimal", "no_think":
		return "no_think"
	case "low":
		return "low"
	default:
		return "high"
	}
}

func hasToolResultBlock(blocks []Block) bool {
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// splitToolContentForCompletions 把工具结果拆成 Chat Completions 可接受的
// role=tool 纯文本，以及必须放到后续 user 消息中的多模态附件。
func splitToolContentForCompletions(blocks []Block, fallback string) (string, []Block) {
	if len(blocks) == 0 {
		return fallback, nil
	}
	var textParts []string
	var media []Block
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "image", "audio":
			media = append(media, b)
		case "tool_result":
			t, nestedMedia := splitToolContentForCompletions(toolResultBlocks(b.Content), "")
			if t != "" {
				textParts = append(textParts, t)
			}
			media = append(media, nestedMedia...)
		}
	}
	text := strings.Join(textParts, "\n")
	if text == "" {
		text = fallback
	}
	return text, media
}

func toolMediaFollowupBlocks(callID string, media []Block) []Block {
	label := "Tool output attachments"
	if callID != "" {
		label += " for call " + callID
	}
	out := make([]Block, 0, len(media)+1)
	out = append(out, Block{Type: "text", Text: label + ":"})
	out = append(out, media...)
	return out
}

// toolResultBlocks 解析 Claude/Responses 工具结果内容，统一保留文本、图片和音频。
func toolResultBlocks(raw json.RawMessage) []Block {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if blocks := parseTextOrBlocks(value); len(blocks) > 0 {
			return blocks
		}
		if value == nil {
			return nil
		}
	}
	return []Block{{Type: "text", Text: string(raw)}}
}

func blocksToOpenAIContent(blocks []Block) any {
	if len(blocks) == 0 {
		return ""
	}
	// 全文本 → 直接字符串
	allText := true
	for _, b := range blocks {
		if b.Type != "text" {
			allText = false
			break
		}
	}
	if allText {
		var s string
		for _, b := range blocks {
			s += b.Text
		}
		return s
	}
	arr := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			arr = append(arr, map[string]any{"type": "text", "text": b.Text})
		case "image":
			arr = append(arr, map[string]any{"type": "image_url", "image_url": map[string]any{"url": b.ImageURL}})
		case "audio":
			_, data, format := audioParts(b.AudioURL)
			if data != "" {
				arr = append(arr, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": data, "format": format}})
			}
		case "tool_result":
			arr = append(arr, map[string]any{"type": "text", "text": rawToText(b.Content)})
		}
	}
	return arr
}

// audioParts 从 data URL 拆出 media_type / data / format（如 wav、mp3）。
func audioParts(url string) (mediaType, data, format string) {
	mediaType, data = splitDataURL(url)
	if data == "" {
		return mediaType, "", ""
	}
	format = strings.TrimPrefix(mediaType, "audio/")
	if format == "" {
		format = "wav"
	}
	return mediaType, data, format
}

func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

// ---------- 序列化到 OpenAI Responses ----------

// buildOpenAIResponsesRequest 把规范化请求序列化为 OpenAI Responses 请求体。
//
// 与 CLIProxyAPI 的 Responses 构造逻辑保持一致（规范要求）：
//   - function_call / function_call_output / reasoning 必须是 input 数组的顶层裸项，
//     绝不能嵌在 message.content 里，否则上游工具循环历史格式错误；
//   - function_call.arguments 是 JSON 字符串（不是对象）；
//   - assistant 文本用 output_text，其他角色用 input_text；
//   - role=tool（chat/completions 工具结果）转顶层 function_call_output；
//   - 只有工具调用的 assistant 消息不输出空 message 项。
func buildOpenAIResponsesRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		out["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	instructions := req.Instructions
	if system := blocksToText(req.System); system != "" {
		if instructions != "" {
			instructions += "\n"
		}
		instructions += system
	}
	if instructions != "" {
		out["instructions"] = instructions
	}
	// 注意：上游（gpt-5.6-luna）rejects include:["usage"]（400），且默认就在
	// response.completed / 非流式 usage 中返回用量，因此这里不再注入 include。
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if rawTC, ok := req.ToolChoice.(json.RawMessage); ok && len(rawTC) > 0 {
		if tc := convertCompletionsToolChoiceToResponses(rawTC); tc != nil {
			out["tool_choice"] = tc
		}
	}
	// input：message 项 + 顶层 function_call / function_call_output / reasoning 裸项
	input := make([]map[string]any, 0, len(req.Messages)+2)
	var content []map[string]any
	curRole := ""
	flush := func() {
		if len(content) == 0 {
			return
		}
		input = append(input, map[string]any{"role": curRole, "content": content})
		content = nil
	}
	for _, m := range req.Messages {
		role := m.Role
		if role == "tool" {
			// chat/completions 的 role=tool 结果 → 顶层 function_call_output
			flush()
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  toolMessageOutput(m),
			})
			continue
		}
		curRole = role
		// m.Text 是 m.Content 中文本块的拼接；若 Content 已含 text 块则不再重复输出
		// （此前会把同一段文本输出两次，上游看到重复内容）。
		hasTextBlock := false
		for _, b := range m.Content {
			if b.Type == "text" {
				hasTextBlock = true
				break
			}
		}
		if m.Text != "" && !hasTextBlock {
			content = append(content, map[string]any{"type": textPartType(role), "text": m.Text})
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				content = append(content, map[string]any{"type": textPartType(role), "text": b.Text})
			case "image":
				content = append(content, map[string]any{"type": "input_image", "image_url": b.ImageURL})
			case "audio":
				_, data, format := audioParts(b.AudioURL)
				if data != "" {
					content = append(content, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": data, "format": format}})
				}
			case "thinking":
				flush()
				input = append(input, map[string]any{
					"type":    "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": b.Thinking}},
				})
			case "tool_use":
				flush()
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   b.ToolUseID,
					"name":      b.Name,
					"arguments": toolArgumentsString(b.Input),
				})
			case "tool_result":
				flush()
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": b.ToolUseID,
					"output":  toolResultOutput(b.Content),
				})
			}
		}
		// 只有工具调用的 assistant 消息不输出空 message 项（对齐 CLIProxyAPI / Responses 规范）
		if role == "assistant" && len(content) == 0 {
			continue
		}
		flush()
	}
	out["input"] = input
	// tools
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			switch t.Type {
			case "function":
				tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.Parameters)})
			case "web_search", "web_search_preview":
				tools = append(tools, map[string]any{"type": t.Type})
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return json.Marshal(out)
}

// textPartType 按角色返回 Responses content part 类型：
// assistant 输出文本用 output_text，其余用 input_text（对齐原生 Responses）。
func textPartType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

// toolArgumentsString 把规范模型中的工具参数（原始 JSON 字节）转成 Responses
// function_call.arguments 要求的 JSON 字符串。
func toolArgumentsString(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	return string(input)
}

// toolMessageOutput 把 role=tool 消息（chat/completions 解析产物）转成
// function_call_output.output：纯文本 → 字符串；含图片/音频 → content parts 数组。
func toolMessageOutput(m Message) any {
	if len(m.Content) == 0 {
		return m.Text
	}
	return blocksToResponsesOutput(m.Content)
}

// toolResultOutput 把工具结果块的原始内容（字符串或 Claude 风格块数组）转成
// function_call_output.output 值。
func toolResultOutput(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return string(raw)
	}
	if raw[0] == '[' {
		var arr []map[string]any
		if json.Unmarshal(raw, &arr) == nil {
			var blocks []Block
			for _, it := range arr {
				typ, _ := it["type"].(string)
				switch typ {
				case "text":
					text, _ := it["text"].(string)
					blocks = append(blocks, Block{Type: "text", Text: text})
				case "image":
					if src, ok := it["source"].(map[string]any); ok {
						if st, _ := src["type"].(string); st == "base64" {
							md, _ := src["media_type"].(string)
							data, _ := src["data"].(string)
							if md != "" && data != "" {
								blocks = append(blocks, Block{Type: "image", ImageURL: "data:" + md + ";base64," + data})
							}
						}
					}
				}
			}
			if out := blocksToResponsesOutput(blocks); out != nil {
				return out
			}
			return ""
		}
	}
	return string(raw)
}

// blocksToResponsesOutput 把块列表转成 function_call_output.output：
// 全文本 → 按行连接的字符串；含图片/音频 → input_text/input_image/input_audio 数组。
func blocksToResponsesOutput(blocks []Block) any {
	if len(blocks) == 0 {
		return nil
	}
	var parts []map[string]any
	allText := true
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
		case "image":
			allText = false
			if b.ImageURL != "" {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": b.ImageURL})
			}
		case "audio":
			allText = false
			_, data, format := audioParts(b.AudioURL)
			if data != "" {
				parts = append(parts, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": data, "format": format}})
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if allText {
		var sb []string
		for _, p := range parts {
			if s, ok := p["text"].(string); ok {
				sb = append(sb, s)
			}
		}
		return strings.Join(sb, "\n")
	}
	return parts
}

// ---------- 序列化到 Anthropic ----------

func buildAnthropicRequest(req *Request) ([]byte, error) {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if req.ThinkingEnabled {
		budget := req.ThinkingBudget
		if budget <= 0 {
			budget = 8000
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	} else if req.ReasoningEffort != "" {
		// openai effort → anthropic thinking（低/中→enabled 小预算，高→大预算）
		budget := 8000
		switch req.ReasoningEffort {
		case "high":
			budget = 32000
		case "medium":
			budget = 8000
		default:
			budget = 2000
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	// system
	if len(req.System) > 0 {
		if len(req.System) == 1 && req.System[0].Type == "text" {
			out["system"] = req.System[0].Text
		} else {
			blocks := make([]map[string]any, 0, len(req.System))
			for _, b := range req.System {
				if b.Type == "text" {
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				}
			}
			out["system"] = blocks
		}
	}
	// messages
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		mm := map[string]any{"role": m.Role}
		if m.Role == "tool" {
			mm["content"] = []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     toolMessageAnthropicContent(m),
			}}
			msgs = append(msgs, mm)
			continue
		}
		content := make([]map[string]any, 0, len(m.Content)+1)
		hasText := false
		if m.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": m.Text})
			hasText = true
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if hasText {
					continue // 已通过 m.Text 输出，避免重复
				}
				content = append(content, map[string]any{"type": "text", "text": b.Text})
			case "thinking":
				content = append(content, map[string]any{"type": "thinking", "thinking": b.Thinking})
			case "image":
				mediaType, data := splitDataURL(b.ImageURL)
				if data != "" {
					content = append(content, map[string]any{
						"type":   "image",
						"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
					})
				}
			case "audio":
				mediaType, data, _ := audioParts(b.AudioURL)
				if data != "" {
					content = append(content, map[string]any{
						"type":   "audio",
						"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
					})
				}
			case "tool_use":
				var inputVal any = map[string]any{}
				if len(b.Input) > 0 {
					_ = json.Unmarshal(b.Input, &inputVal)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": b.ToolUseID, "name": b.Name, "input": inputVal})
			case "tool_result":
				content = append(content, map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID, "content": rawToText(b.Content)})
			}
		}
		if len(content) > 0 {
			mm["content"] = content
		} else {
			mm["content"] = m.Text
		}
		msgs = append(msgs, mm)
	}
	out["messages"] = msgs
	// tools
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type == "function" {
				tools = append(tools, map[string]any{
					"name":         t.Name,
					"description":  t.Description,
					"input_schema": json.RawMessage(t.Parameters),
				})
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return json.Marshal(out)
}

func splitDataURL(url string) (mediaType, data string) {
	const prefix = "data:"
	if len(url) < len(prefix) || url[:len(prefix)] != prefix {
		return "image/png", url
	}
	rest := url[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ';' {
			mt := rest[:i]
			if i+7 < len(rest) && rest[i:i+8] == ";base64," {
				return mt, rest[i+8:]
			}
			break
		}
	}
	return "image/png", rest
}

// toolMessageAnthropicContent 把 role=tool 消息（chat/completions 解析产物）转成
// anthropic tool_result.content：纯文本 → 字符串；含图片/音频 → 块数组。
// 注意不能直接用 m.Content[0].Content：文本块的正文在 Block.Text，空内容会 panic。
func toolMessageAnthropicContent(m Message) any {
	if len(m.Content) == 0 {
		return m.Text
	}
	var parts []map[string]any
	allText := true
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case "image":
			allText = false
			mediaType, data := splitDataURL(b.ImageURL)
			if data != "" {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}})
			}
		case "audio":
			allText = false
			mediaType, data, _ := audioParts(b.AudioURL)
			if data != "" {
				parts = append(parts, map[string]any{"type": "audio", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}})
			}
		}
	}
	if allText {
		var sb []string
		for _, p := range parts {
			if s, ok := p["text"].(string); ok {
				sb = append(sb, s)
			}
		}
		return strings.Join(sb, "\n")
	}
	return parts
}

// toolResultText 从工具结果块内容提取纯文本：
// - JSON 字符串 → 原字符串
// - 数组（Claude tool_result.content 多块，含 text/image）→ 提取 text 块按行连接
// - 其他 → 原样字符串
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	if raw[0] == '[' {
		var arr []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &arr) == nil {
			var sb []string
			for _, it := range arr {
				if it.Type == "text" && it.Text != "" {
					sb = append(sb, it.Text)
				}
			}
			if len(sb) > 0 {
				return strings.Join(sb, "\n")
			}
		}
	}
	return string(raw)
}

// joinSystemBlocks 把 system/instructions 块拼成 chat/completions content：
// 全文本时按块用 "\n\n" 连接；含图片等非文本块时走标准多模态数组。
func joinSystemBlocks(blocks []Block) any {
	allText := len(blocks) > 0
	for _, b := range blocks {
		if b.Type != "text" {
			allText = false
			break
		}
	}
	if allText {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return blocksToOpenAIContent(blocks)
}

// convertResponsesToolChoiceToCompletions 把 Responses 格式 tool_choice 转 chat/completions 格式。
// 字符串（"auto"/"none"/"required"）原样保留；function/custom 对象转嵌套 function 形式；
// 其他 built-in（web_search 等）返回 nil（completions 不支持，丢弃）。
func convertResponsesToolChoiceToCompletions(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	switch obj.Type {
	case "function", "custom":
		if obj.Name == "" {
			return nil
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": obj.Name},
		}
	}
	return nil
}

// convertCompletionsToolChoiceToResponses 把 chat/completions 格式 tool_choice 转 Responses 格式。
// 字符串原样保留；{"type":"function","function":{"name":"X"}} 展平为 {"type":"function","name":"X"}。
func convertCompletionsToolChoiceToResponses(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	name := obj.Name
	if name == "" && obj.Function != nil {
		name = obj.Function.Name
	}
	if obj.Type == "function" && name != "" {
		return map[string]any{"type": "function", "name": name}
	}
	// 其他 built-in 保持原样
	return json.RawMessage(raw)
}
