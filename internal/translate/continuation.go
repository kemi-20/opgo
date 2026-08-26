package translate

import (
	"strings"
	"unicode/utf8"
)

const maxPreludeContinuations = 3

// shouldContinueAfterPrelude 识别模型只输出“现在让我读取……”之类过渡句、却以
// stop 结束且没有真正发出工具调用的情况。Responses/Codex 可用 end_turn=false
// 要求继续采样；Chat Completions 没有对应字段，所以转换层需要保守补齐语义。
//
// 规则刻意收窄：必须配置了工具、文本较短、同时包含继续标记和动作词；并且同一
// 用户轮次最多救援三次，避免能力较弱的模型反复输出过渡句形成无限循环。
func shouldContinueAfterPrelude(text string, meta *Request) bool {
	if meta == nil || len(meta.Tools) == 0 {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 500 {
		return false
	}
	if !looksLikeUnfinishedPrelude(text) {
		return false
	}

	// 找到最近一次用户输入。同一用户轮次内的后续采样都受同一个上限约束；
	// 新的用户消息会重置计数，不会污染后续独立对话。
	lastUser := -1
	for i := len(meta.Messages) - 1; i >= 0; i-- {
		if meta.Messages[i].Role == "user" {
			lastUser = i
			break
		}
	}

	seen := 0
	for i := lastUser + 1; i < len(meta.Messages); i++ {
		message := meta.Messages[i]
		candidate := message.Text
		if candidate == "" {
			candidate = blocksText(message.Content)
		}
		if message.Role == "assistant" && looksLikeUnfinishedPrelude(candidate) {
			seen++
		}
	}
	return seen < maxPreludeContinuations
}

func looksLikeUnfinishedPrelude(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" || utf8.RuneCountInString(t) > 500 {
		return false
	}

	// “请让我知道/告诉我”通常是正常收尾，不应触发续跑。
	for _, closing := range []string{"让我知道", "让我了解", "告诉我即可", "随时告诉我", "let me know"} {
		if strings.Contains(t, closing) {
			return false
		}
	}

	marker := containsAny(t, []string{
		"现在让我", "现在我来", "接下来", "下一步", "下面我", "然后我", "让我来", "我来继续",
		"now let me", "now i'll", "now i will", "next i'll", "next i will", "let me now",
	})
	action := containsAny(t, []string{
		"调用", "执行", "测试", "检查", "尝试", "继续", "开始", "进行", "看看", "分析", "验证",
		"整理", "汇总", "报告", "处理", "修复", "搜索", "读取", "收集", "列出", "展开", "分批",
		" call", " run", " test", " check", " try", " continue", " inspect", " read", " search",
		" analyze", " verify", " collect", " summarize", " report",
	})
	return marker && action
}

func containsAny(s string, values []string) bool {
	for _, value := range values {
		if strings.Contains(s, value) {
			return true
		}
	}
	return false
}
