package proxy

import (
	"bytes"
	"encoding/json"
)

const museSparkModel = "muse-spark-1.2-contributor"

// sanitizeMuseResponsesTools applies the one compatibility exception required by
// Muse's native Responses endpoint. OpenCodex currently adds
// search_content_types to web_search, while that endpoint accepts web_search but
// rejects that field. The model name, upstream wire format, tool type and field
// must all match; every other request is returned byte-for-byte unchanged.
func sanitizeMuseResponsesTools(model string, upstreamFormat string, body []byte) ([]byte, bool) {
	if model != museSparkModel || upstreamFormat != "openai_responses" {
		return body, false
	}

	toolsStart, toolsEnd, ok := topLevelJSONField(body, "tools")
	if !ok {
		return body, false
	}

	sanitizedTools, changed := sanitizeMuseToolsArray(body[toolsStart:toolsEnd])
	if !changed {
		return body, false
	}
	out := make([]byte, 0, len(body)-(toolsEnd-toolsStart)+len(sanitizedTools))
	out = append(out, body[:toolsStart]...)
	out = append(out, sanitizedTools...)
	out = append(out, body[toolsEnd:]...)
	return out, true
}

type jsonSpan struct{ start, end int }

// sanitizeMuseToolsArray retains every byte in the tools array except the exact
// incompatible property. It deliberately does not decode/re-encode the request,
// so ordering, whitespace, escaped text, numeric forms and unrelated tools stay
// byte-identical.
func sanitizeMuseToolsArray(raw []byte) ([]byte, bool) {
	spans, ok := jsonArrayElementSpans(raw)
	if !ok {
		return raw, false
	}
	var replacements []struct {
		jsonSpan
		value []byte
	}
	for _, span := range spans {
		toolRaw := raw[span.start:span.end]
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(toolRaw, &header) != nil || header.Type != "web_search" {
			continue
		}
		cleaned, changed := removeTopLevelJSONObjectField(toolRaw, "search_content_types")
		if changed {
			replacements = append(replacements, struct {
				jsonSpan
				value []byte
			}{span, cleaned})
		}
	}
	if len(replacements) == 0 {
		return raw, false
	}
	var out bytes.Buffer
	cursor := 0
	for _, replacement := range replacements {
		out.Write(raw[cursor:replacement.start])
		out.Write(replacement.value)
		cursor = replacement.end
	}
	out.Write(raw[cursor:])
	return out.Bytes(), true
}

func jsonArrayElementSpans(raw []byte) ([]jsonSpan, bool) {
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return nil, false
	}
	i++
	var spans []jsonSpan
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) {
			return nil, false
		}
		if raw[i] == ']' {
			return spans, true
		}
		end, ok := scanJSONValue(raw, i)
		if !ok {
			return nil, false
		}
		spans = append(spans, jsonSpan{i, end})
		i = skipJSONSpace(raw, end)
		if i >= len(raw) {
			return nil, false
		}
		switch raw[i] {
		case ',':
			i++
		case ']':
			return spans, true
		default:
			return nil, false
		}
	}
}

func removeTopLevelJSONObjectField(raw []byte, wanted string) ([]byte, bool) {
	changedAny := false
	for {
		cleaned, changed := removeOneTopLevelJSONObjectField(raw, wanted)
		if !changed {
			return raw, changedAny
		}
		changedAny = true
		raw = cleaned
		// A valid object normally has unique keys, but remove duplicates too so a
		// crafted request cannot leave one incompatible copy behind.
		if _, _, exists := topLevelJSONField(raw, wanted); !exists {
			return raw, changedAny
		}
	}
}

func removeOneTopLevelJSONObjectField(raw []byte, wanted string) ([]byte, bool) {
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return raw, false
	}
	i++
	previousComma := -1
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) || raw[i] == '}' {
			return raw, false
		}
		keyStart := i
		keyEnd, ok := scanJSONString(raw, keyStart)
		if !ok {
			return raw, false
		}
		var key string
		if json.Unmarshal(raw[keyStart:keyEnd], &key) != nil {
			return raw, false
		}
		i = skipJSONSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return raw, false
		}
		valueStart := skipJSONSpace(raw, i+1)
		valueEnd, ok := scanJSONValue(raw, valueStart)
		if !ok {
			return raw, false
		}
		afterValue := skipJSONSpace(raw, valueEnd)
		if afterValue >= len(raw) {
			return raw, false
		}
		commaAfter := -1
		if raw[afterValue] == ',' {
			commaAfter = afterValue
		} else if raw[afterValue] != '}' {
			return raw, false
		}
		if key == wanted {
			removeStart, removeEnd := keyStart, valueEnd
			if commaAfter >= 0 {
				removeEnd = commaAfter + 1
			} else if previousComma >= 0 {
				removeStart = previousComma
			}
			out := make([]byte, 0, len(raw)-(removeEnd-removeStart))
			out = append(out, raw[:removeStart]...)
			out = append(out, raw[removeEnd:]...)
			return out, true
		}
		if commaAfter < 0 {
			return raw, false
		}
		previousComma = commaAfter
		i = commaAfter + 1
	}
}

// topLevelJSONField returns the byte range occupied by a top-level object's
// value. It lets the Muse fix replace only tools while retaining every byte
// outside that value, including unknown fields and their original number forms.
func topLevelJSONField(raw []byte, wanted string) (int, int, bool) {
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return 0, 0, false
	}
	i++
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) || raw[i] == '}' {
			return 0, 0, false
		}
		keyStart := i
		keyEnd, ok := scanJSONString(raw, keyStart)
		if !ok {
			return 0, 0, false
		}
		var key string
		if json.Unmarshal(raw[keyStart:keyEnd], &key) != nil {
			return 0, 0, false
		}
		i = skipJSONSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return 0, 0, false
		}
		valueStart := skipJSONSpace(raw, i+1)
		valueEnd, ok := scanJSONValue(raw, valueStart)
		if !ok {
			return 0, 0, false
		}
		if key == wanted {
			return valueStart, valueEnd, true
		}
		i = skipJSONSpace(raw, valueEnd)
		if i >= len(raw) {
			return 0, 0, false
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			return 0, 0, false
		default:
			return 0, 0, false
		}
	}
}

func skipJSONSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func scanJSONString(raw []byte, i int) (int, bool) {
	if i >= len(raw) || raw[i] != '"' {
		return 0, false
	}
	for i = i + 1; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
			if i >= len(raw) {
				return 0, false
			}
		case '"':
			return i + 1, true
		}
	}
	return 0, false
}

func scanJSONValue(raw []byte, i int) (int, bool) {
	if i >= len(raw) {
		return 0, false
	}
	if raw[i] == '"' {
		return scanJSONString(raw, i)
	}
	if raw[i] == '{' || raw[i] == '[' {
		open := raw[i]
		close := byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 1
		for i = i + 1; i < len(raw); i++ {
			if raw[i] == '"' {
				end, ok := scanJSONString(raw, i)
				if !ok {
					return 0, false
				}
				i = end - 1
				continue
			}
			if raw[i] == open {
				depth++
			} else if raw[i] == close {
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	}
	start := i
	for i < len(raw) {
		switch raw[i] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
			return i, i > start
		default:
			i++
		}
	}
	return i, i > start
}
