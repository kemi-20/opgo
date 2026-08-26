package proxy

import "encoding/json"

// replaceUpstreamModel 只替换 JSON 顶层 model 字段。这里不做整体反序列化/
// 重序列化，避免改变字段顺序、数字书写形式或客户端传入的其他未知字段。
func replaceUpstreamModel(body []byte, providerModel string) ([]byte, bool, error) {
	if providerModel == "" {
		return body, false, nil
	}
	modelValue, err := json.Marshal(providerModel)
	if err != nil {
		return body, false, err
	}
	valueStart, valueEnd, ok := topLevelJSONField(body, "model")
	if !ok {
		return body, false, nil
	}
	out := make([]byte, 0, len(body)-valueEnd+valueStart+len(modelValue))
	out = append(out, body[:valueStart]...)
	out = append(out, modelValue...)
	out = append(out, body[valueEnd:]...)
	return out, true, nil
}

// maskProviderModelInJSON 将 JSON 中所有等于 providerModel 的 model 字段值
// 改回公开模型名。这样透传响应、嵌套 response 对象和流式事件都不会泄露别名。
func maskProviderModelInJSON(body []byte, providerModel, publicModel string) []byte {
	if providerModel == "" || providerModel == publicModel {
		return body
	}
	type edit struct {
		start, end int
		value      []byte
	}
	var edits []edit
	_, err := json.Marshal(providerModel)
	if err != nil {
		return body
	}
	toValue, err := json.Marshal(publicModel)
	if err != nil {
		return body
	}

	for i := 0; i < len(body); {
		if body[i] != '"' {
			i++
			continue
		}
		keyStart := i
		keyEnd, ok := scanJSONString(body, keyStart)
		if !ok {
			i++
			continue
		}
		var key string
		if json.Unmarshal(body[keyStart:keyEnd], &key) != nil {
			i = keyEnd
			continue
		}
		j := skipJSONSpace(body, keyEnd)
		if j >= len(body) || body[j] != ':' {
			i = keyEnd
			continue
		}
		valueStart := skipJSONSpace(body, j+1)
		valueEnd, ok := scanJSONString(body, valueStart)
		if !ok {
			// 非字符串值（对象、数组、数字）也可能包含嵌套 model 字段；
			// 从当前字符继续扫描即可进入这些子结构。
			i++
			continue
		}
		if key == "model" {
			var value string
			if json.Unmarshal(body[valueStart:valueEnd], &value) == nil && value == providerModel {
				edits = append(edits, edit{start: valueStart, end: valueEnd, value: toValue})
			}
		}
		i = valueEnd
	}
	if len(edits) == 0 {
		return body
	}
	for i := len(edits) - 1; i >= 0; i-- {
		item := edits[i]
		updated := make([]byte, 0, len(body)-(item.end-item.start)+len(item.value))
		updated = append(updated, body[:item.start]...)
		updated = append(updated, item.value...)
		updated = append(updated, body[item.end:]...)
		body = updated
	}
	return body
}
