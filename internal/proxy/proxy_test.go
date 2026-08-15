package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/store"
)

const (
	testMaster = "sk-MASTER-TEST-KEY-000000000000"
	testAdmin  = "admin-pw-123"
	testUser1  = "sk-user-1-key-1111111111"
	testUser2  = "sk-user-2-key-2222222222"
)

func testConfigJSON(upstream string) string {
	return fmt.Sprintf(`{
		"listen": ":0",
		"upstream_base": %q,
		"master_key": "sk-MASTER-TEST-KEY-000000000000",
		"admin_password": "admin-pw-123",
		"rate_limit_per_minute": 0,
		"limits_per_user": {"5h": 2.4, "1w": 6.0, "1m": 12.0},
		"pricing": {
			"deepseek-v4-flash": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0},
			"mimo-v2.5": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0}
		},
		"users": [
			{"uuid": "uuid-1", "remark": "张三", "keys": ["sk-user-1-key-1111111111"]},
			{"uuid": "uuid-2", "remark": "", "keys": ["sk-user-2-key-2222222222"]}
		]
	}`, upstream)
}

func newTestProxy(t *testing.T, upstream string, bal BalanceSource, mutate func(*config.Config)) (*Proxy, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfigJSON(upstream)))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
	}
	db, err := store.Open(t.TempDir() + "/usage.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, db, []byte("<html>test</html>"), bal, log), db
}

type fakeUpstream struct {
	mu       sync.Mutex
	lastAuth string
	lastBody []byte
	stream   bool
	status   int
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		f.lastBody, _ = io.ReadAll(r.Body)
		st := f.status
		if st == 0 {
			st = 200
		}
		f.mu.Unlock()
		if f.stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"prompt_tokens_details\":{\"cached_tokens\":10}}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		fmt.Fprint(w, "{\"id\":\"x\",\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"prompt_tokens_details\":{\"cached_tokens\":10}}}")
	})
}

func newFakeServer(f *fakeUpstream) *httptest.Server {
	return httptest.NewServer(f.handler())
}

func (f *fakeUpstream) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

type fixedBalance struct{ snap *balance.Snapshot }

func (f *fixedBalance) Snapshot() (*balance.Snapshot, bool) {
	cp := *f.snap
	return &cp, true
}

type noBalance struct{}

func (n *noBalance) Snapshot() (*balance.Snapshot, bool) { return nil, false }

func okSnapshot() *balance.Snapshot {
	now := time.Now().UTC()
	return &balance.Snapshot{
		FetchedAt: now,
		Rolling:   balance.CapInfo{Status: "ok", Percent: 5, ResetsAt: now.Add(2 * time.Hour)},
		Weekly:    balance.CapInfo{Status: "ok", Percent: 0, ResetsAt: now.Add(24 * time.Hour)},
		Monthly:   balance.CapInfo{Status: "ok", Percent: 0, ResetsAt: now.Add(24 * time.Hour)},
	}
}

func doReq(t *testing.T, srv *httptest.Server, method, path, auth, body string) (int, string, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

const chatBody = "{\"model\":\"mimo-v2.5\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"

func TestForwardNonStream(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	if got := fu.auth(); got != "Bearer "+testMaster {
		t.Errorf("上游收到 %q", got)
	}
	// 90*0.14/1e6 + 10*0.0028/1e6 + 50*0.28/1e6 = 2663 微元
	sum, err := db.UserWindowSum("uuid-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 2663 {
		t.Errorf("sum = %d, want 2663", sum)
	}
}

func TestForwardStream(t *testing.T) {
	fu := &fakeUpstream{stream: true}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	body := "{\"model\":\"mimo-v2.5\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
	status, respBody, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, body)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, respBody)
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Error("流应完整透传")
	}
	if !strings.Contains(string(fu.lastBody), "include_usage") {
		t.Error("应注入 stream_options.include_usage")
	}
	sum, _ := db.UserWindowSum("uuid-1", 0)
	if sum != 2663 {
		t.Errorf("sum = %d, want 2663", sum)
	}
}

func TestAnthropicNoInjection(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	body := "{\"model\":\"deepseek-v4-flash\",\"stream\":true,\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
	status, _, _ := doReq(t, srv, "POST", "/v1/messages", testUser1, body)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if strings.Contains(string(fu.lastBody), "stream_options") {
		t.Error("/messages 不应注入 stream_options")
	}
}

func TestAuth401(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	if status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", "", chatBody); status != 401 {
		t.Errorf("无鉴权 status = %d: %s", status, body)
	}
	if status, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", "sk-wrong", chatBody); status != 401 {
		t.Errorf("错 key status = %d", status)
	}
}

func TestUnknownModel403(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1,
		"{\"model\":\"gpt-x\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
	if status != 403 {
		t.Errorf("status = %d: %s", status, body)
	}
}

func TestUserLimit429(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, func(c *config.Config) {
		c.LimitsPerUser = map[string]float64{"5h": 1e-8, "1w": 1e-8, "1m": 1e-8}
	})
	srv := httptest.NewServer(p)
	defer srv.Close()

	if status, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); status != 200 {
		t.Fatalf("首次请求 status = %d", status)
	}
	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 429 {
		t.Fatalf("第二次 status = %d", status)
	}
	if !strings.Contains(body, "个人额度") {
		t.Errorf("应提示个人额度: %s", body)
	}
}

func TestPoolPercent429(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	snap := okSnapshot()
	snap.Rolling.Percent = 100
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: snap}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 429 || !strings.Contains(body, "总池额度") {
		t.Errorf("status = %d: %s", status, body)
	}
}

func TestPoolStatusNotOK429(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	snap := okSnapshot()
	snap.Weekly.Status = "blocked"
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: snap}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 429 || !strings.Contains(body, "总池额度") {
		t.Errorf("status = %d: %s", status, body)
	}
}

func TestNotSynced503(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &noBalance{}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 503 {
		t.Errorf("status = %d: %s", status, body)
	}
}

func TestRateLimit429(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, func(c *config.Config) {
		c.RateLimitPerMinute = 2
	})
	srv := httptest.NewServer(p)
	defer srv.Close()

	if status, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); status != 200 {
		t.Fatalf("第1次 status = %d", status)
	}
	if status, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); status != 200 {
		t.Fatalf("第2次 status = %d", status)
	}
	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 429 || !strings.Contains(body, "频繁") {
		t.Errorf("第3次 status = %d: %s", status, body)
	}
}

func TestModelsList(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "GET", "/v1/models", testUser1, "")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	var obj struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Data) != 2 || obj.Data[0].ID != "deepseek-v4-flash" || obj.Data[1].ID != "mimo-v2.5" {
		t.Errorf("models = %+v", obj.Data)
	}
	if status, _, _ := doReq(t, srv, "GET", "/v1/models", "", ""); status != 401 {
		t.Errorf("未认证 status = %d", status)
	}
}

func TestAPIUsage(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)

	status, body, _ := doReq(t, srv, "POST", "/api/usage", "", "{\"key\":\"sk-user-1-key-1111111111\"}")
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["uuid"] != "uuid-1" {
		t.Errorf("uuid = %v", obj["uuid"])
	}
	if _, ok := obj["remark"]; ok {
		t.Error("api/usage 不应包含 remark")
	}
	if _, ok := obj["keys"]; ok {
		t.Error("api/usage 不应包含 keys")
	}
	if synced, _ := obj["synced"].(bool); !synced {
		t.Error("synced 应为 true")
	}
	windows, _ := obj["windows"].(map[string]any)
	if windows == nil {
		t.Fatal("无 windows")
	}
	w5, _ := windows["5h"].(map[string]any)
	if w5 == nil || w5["used"].(float64) <= 0 {
		t.Errorf("5h used 应为正: %v", w5)
	}
	total, _ := obj["total"].(map[string]any)
	if total == nil || total["5h"] == nil {
		t.Error("无 total")
	}
	if status, _, _ := doReq(t, srv, "POST", "/api/usage", "", "{\"key\":\"sk-wrong\"}"); status != 401 {
		t.Errorf("错 key status = %d", status)
	}
}

func TestAPIAdmin(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	if status, _, _ := doReq(t, srv, "POST", "/api/admin", "", "{\"password\":\"wrong\"}"); status != 401 {
		t.Errorf("错密码 status = %d", status)
	}
	status, body, _ := doReq(t, srv, "POST", "/api/admin", "", "{\"password\":\"admin-pw-123\"}")
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	var obj struct {
		Users []struct {
			UUID   string `json:"uuid"`
			Remark string `json:"remark"`
			Keys   any    `json:"keys"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Users) != 2 || obj.Users[0].UUID != "uuid-1" || obj.Users[0].Remark != "张三" {
		t.Errorf("users = %+v", obj.Users)
	}
	for _, u := range obj.Users {
		if u.Keys != nil {
			t.Error("admin 响应不应包含 keys")
		}
	}
}

func TestHealthz(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	status, body, _ := doReq(t, srv, "GET", "/healthz", "", "")
	if status != 200 || !strings.Contains(body, "ok") {
		t.Errorf("status = %d: %s", status, body)
	}
}

func TestNoKeyLeak(t *testing.T) {
	allKeys := []string{testMaster, testUser1, testUser2}
	do := func(name, body string, hdr http.Header) {
		t.Helper()
		for _, k := range allKeys {
			if strings.Contains(body, k) {
				t.Errorf("%s 响应体包含 key %s", name, k)
				return
			}
			for _, vs := range hdr {
				for _, v := range vs {
					if strings.Contains(v, k) {
						t.Errorf("%s 响应头包含 key %s", name, k)
						return
					}
				}
			}
		}
	}

	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()

	p1, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	s1 := httptest.NewServer(p1)
	defer s1.Close()
	p2, _ := newTestProxy(t, up.URL, &noBalance{}, nil)
	s2 := httptest.NewServer(p2)
	defer s2.Close()
	p3, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, func(c *config.Config) {
		c.LimitsPerUser = map[string]float64{"5h": 1e-8, "1w": 1e-8, "1m": 1e-8}
	})
	s3 := httptest.NewServer(p3)
	defer s3.Close()
	p4, _ := newTestProxy(t, "http://127.0.0.1:1", &fixedBalance{snap: okSnapshot()}, nil)
	s4 := httptest.NewServer(p4)
	defer s4.Close()

	_, b, h := doReq(t, s1, "GET", "/", "", "")
	do("首页", b, h)
	_, b, h = doReq(t, s1, "GET", "/healthz", "", "")
	do("healthz", b, h)
	_, b, h = doReq(t, s1, "POST", "/api/usage", "", "{\"key\":\"sk-user-1-key-1111111111\"}")
	do("api/usage", b, h)
	_, b, h = doReq(t, s1, "POST", "/api/usage", "", "{\"key\":\"sk-wrong\"}")
	do("api/usage-401", b, h)
	_, b, h = doReq(t, s1, "POST", "/api/admin", "", "{\"password\":\"wrong\"}")
	do("api/admin-401", b, h)
	_, b, h = doReq(t, s1, "POST", "/api/admin", "", "{\"password\":\"admin-pw-123\"}")
	do("api/admin", b, h)
	_, b, h = doReq(t, s1, "GET", "/v1/models", testUser1, "")
	do("models", b, h)
	_, b, h = doReq(t, s1, "GET", "/v1/models", "", "")
	do("models-401", b, h)
	_, b, h = doReq(t, s1, "POST", "/v1/chat/completions", testUser1, chatBody)
	do("forward", b, h)
	_, b, h = doReq(t, s1, "POST", "/v1/chat/completions", testUser1, "{\"model\":\"unknown-model\",\"messages\":[]}")
	do("403", b, h)
	_, b, h = doReq(t, s1, "POST", "/v1/chat/completions", "", chatBody)
	do("401", b, h)
	_, b, h = doReq(t, s2, "POST", "/v1/chat/completions", testUser1, chatBody)
	do("503", b, h)
	doReq(t, s3, "POST", "/v1/chat/completions", testUser1, chatBody)
	_, b, h = doReq(t, s3, "POST", "/v1/chat/completions", testUser1, chatBody)
	do("429", b, h)
	_, b, h = doReq(t, s4, "POST", "/v1/chat/completions", testUser1, chatBody)
	do("502", b, h)

	// JSON 错误响应必须是 application/json（防 HTML 解析）
	for _, p := range []struct {
		srv  *httptest.Server
		path string
	}{{s1, "/api/usage"}, {s1, "/v1/chat/completions"}} {
		req, _ := http.NewRequest("POST", p.srv.URL+p.path, strings.NewReader(chatBody))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s Content-Type = %q", p.path, ct)
		}
	}
}
