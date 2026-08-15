package balance

import (
	"strings"
	"testing"
	"time"
)

const realSample = `{"usage":{"rolling":{"status":"ok","percent":2,"resetsAt":"2026-08-15T05:49:57.457Z"},"weekly":{"status":"ok","percent":0,"resetsAt":"2026-08-17T00:00:00.457Z"},"monthly":{"status":"ok","percent":0,"resetsAt":"2026-09-15T00:43:29.457Z"}}}`

func TestParseRealSample(t *testing.T) {
	s, err := Parse([]byte(realSample))
	if err != nil {
		t.Fatal(err)
	}
	if s.Rolling.Percent != 2 || s.Rolling.Status != "ok" {
		t.Errorf("rolling = %+v", s.Rolling)
	}
	if s.Rolling.ResetsAt.Format(time.RFC3339) != "2026-08-15T05:49:57Z" {
		t.Errorf("rolling resetsAt = %v", s.Rolling.ResetsAt)
	}
	if s.Weekly.Percent != 0 || s.Monthly.Percent != 0 {
		t.Errorf("weekly/monthly = %+v %+v", s.Weekly, s.Monthly)
	}
	start := s.Rolling.WindowStart(5 * time.Hour)
	if !start.Equal(s.Rolling.ResetsAt.Add(-5 * time.Hour)) {
		t.Errorf("window start = %v", start)
	}
}

func TestParseMissingCap(t *testing.T) {
	body := `{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"2026-08-15T05:49:57Z"}}}`
	if _, err := Parse([]byte(body)); err == nil {
		t.Error("缺额度应失败")
	}
}

func TestParseBadJSON(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("坏 JSON 应失败")
	}
}

func TestParseBadTime(t *testing.T) {
	body := strings.Replace(realSample, "2026-08-15T05:49:57.457Z", "nope", 1)
	if _, err := Parse([]byte(body)); err == nil {
		t.Error("非法 resetsAt 应失败")
	}
}

func TestParseBadPercent(t *testing.T) {
	body := strings.Replace(realSample, "\"percent\":2", "\"percent\":-1", 1)
	if _, err := Parse([]byte(body)); err == nil {
		t.Error("非法 percent 应失败")
	}
}

func TestExceeded(t *testing.T) {
	if (CapInfo{Status: "ok", Percent: 99}).Exceeded() {
		t.Error("99% 不应超限")
	}
	if !(CapInfo{Status: "ok", Percent: 100}).Exceeded() {
		t.Error("100% 应超限")
	}
	if !(CapInfo{Status: "blocked", Percent: 50}).Exceeded() {
		t.Error("status 非 ok 应超限")
	}
}

func TestCapMapping(t *testing.T) {
	s := Snapshot{Rolling: CapInfo{Percent: 1}, Weekly: CapInfo{Percent: 2}, Monthly: CapInfo{Percent: 3}}
	if s.Cap("5h").Percent != 1 || s.Cap("1w").Percent != 2 || s.Cap("1m").Percent != 3 {
		t.Error("Cap 映射错误")
	}
}
