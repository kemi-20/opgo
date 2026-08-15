package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/meter"
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
			"deepseek-v4-flash": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 1048576},
			"deepseek-v4-pro": {"input_per_million": 0.56, "output_per_million": 1.12, "cached_read_per_million": 0.0112, "cached_write_per_million": 0, "context_length": 1048576},
			"mimo-v2.5": {"input_per_million": 0.14, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 1048576}
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
	mgr := config.NewManager(cfg, "", nil, log)
	return New(mgr, db, []byte("<html>test</html>"), bal, log), db
}

type fakeUpstream struct {
	mu        sync.Mutex
	lastAuth  string
	lastPath  string
	lastBody  []byte
	lastUA    string
	lastXTest string
	stream    bool
	status    int
	zeroUsage bool
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		f.lastPath = r.URL.Path
		f.lastBody, _ = io.ReadAll(r.Body)
		f.lastUA = r.Header.Get("User-Agent")
		f.lastXTest = r.Header.Get("X-Test-Custom")
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
		if f.zeroUsage {
			fmt.Fprint(w, "{\"id\":\"x\",\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}")
			return
		}
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

func (f *fakeUpstream) path() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPath
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
	return doReqH(t, srv, method, path, auth, body, nil)
}

// doReqH 同 doReq，但可附加自定义请求头（用于验证透传）。
func doReqH(t *testing.T, srv *httptest.Server, method, path, auth, body string, headers map[string]string) (int, string, http.Header) {
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
	for k, v := range headers {
		req.Header.Set(k, v)
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
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Data) != 3 || obj.Data[0].ID != "deepseek-v4-flash" || obj.Data[1].ID != "deepseek-v4-pro" || obj.Data[2].ID != "mimo-v2.5" {
		t.Errorf("models = %+v", obj.Data)
	}
	for i, m := range obj.Data {
		if m.ContextLength != 1048576 {
			t.Errorf("models[%d] context_length = %d，应 1048576", i, m.ContextLength)
		}
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

func TestV1PathRewrite(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	if got := fu.path(); got != "/chat/completions" {
		t.Errorf("上游收到路径 %q，期望 /chat/completions", got)
	}
	// 无 /v1 前缀的路径原样透传
	doReq(t, srv, "POST", "/messages", testUser1, chatBody)
	if got := fu.path(); got != "/messages" {
		t.Errorf("上游收到路径 %q，期望 /messages", got)
	}
}

func TestUsageEndpoint(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "GET", "/v1/usage", testUser1, "")
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	var obj struct {
		Usage struct {
			Rolling map[string]any `json:"rolling"`
			Weekly  map[string]any `json:"weekly"`
			Monthly map[string]any `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]map[string]any{"rolling": obj.Usage.Rolling, "weekly": obj.Usage.Weekly, "monthly": obj.Usage.Monthly} {
		if c == nil {
			t.Fatalf("缺少 %s", name)
		}
		for _, f := range []string{"status", "percent", "resetsAt"} {
			if _, ok := c[f]; !ok {
				t.Errorf("%s 缺少字段 %s", name, f)
			}
		}
	}
	if _, ok := obj.Usage.Rolling["resets_at"]; ok {
		t.Error("不应出现 resets_at（官方字段名是 resetsAt）")
	}
	if status, _, _ := doReq(t, srv, "GET", "/v1/usage", "", ""); status != 401 {
		t.Errorf("未认证 status = %d", status)
	}
	// 余额未同步 → 503
	p2, _ := newTestProxy(t, up.URL, &noBalance{}, nil)
	srv2 := httptest.NewServer(p2)
	defer srv2.Close()
	if status, _, _ := doReq(t, srv2, "GET", "/v1/usage", testUser1, ""); status != 503 {
		t.Errorf("未同步 status = %d", status)
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
	_, b, h = doReq(t, s1, "GET", "/v1/usage", testUser1, "")
	do("v1/usage", b, h)
	_, b, h = doReq(t, s1, "GET", "/v1/usage", "", "")
	do("v1/usage-401", b, h)
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

func TestModelsListOmitsContextLengthWhenUnset(t *testing.T) {
	up := newFakeServer(&fakeUpstream{})
	defer up.Close()
	// 清除所有模型的 context_length，模拟 config 未定义
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, func(c *config.Config) {
		for name, pr := range c.Pricing {
			pr.ContextLength = 0
			c.Pricing[name] = pr
		}
	})
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, body, _ := doReq(t, srv, "GET", "/v1/models", testUser1, "")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if strings.Contains(body, "context_length") {
		t.Errorf("未配置 context_length 时 /models 不应包含该字段，body = %s", body)
	}
	var obj struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Data) != 3 {
		t.Errorf("models = %+v", obj.Data)
	}
}

func hotConfig(upstream, master, userKey, model, price string) string {
	return fmt.Sprintf(`{
		"listen": ":0",
		"upstream_base": %q,
		"master_key": %q,
		"admin_password": "admin-pw-123",
		"rate_limit_per_minute": 0,
		"limits_per_user": {"5h": 2.4, "1w": 6.0, "1m": 12.0},
		"pricing": {
			%q: {"input_per_million": %s, "output_per_million": 0.28, "cached_read_per_million": 0.0028, "cached_write_per_million": 0, "context_length": 1048576}
		},
		"users": [
			{"uuid": "uuid-hot", "remark": "", "keys": [%q]}
		]
	}`, upstream, master, model, price, userKey)
}

// TestHotReloadConfig 验证：改配置文件后 Reload，users/pricing/master_key/models 即时生效，
// 旧 key 立即失效，无需重启。
func TestHotReloadConfig(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	vA := hotConfig(up.URL, "sk-MASTER-A", "sk-key-a", "mimo-v2.5", "0.14")
	if err := os.WriteFile(path, []byte(vA), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(vA))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := config.NewManager(cfg, path, nil, log)
	db, err := store.Open(t.TempDir() + "/usage.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := New(mgr, db, []byte("<html>test</html>"), &fixedBalance{snap: okSnapshot()}, log)
	srv := httptest.NewServer(p)
	defer srv.Close()

	// 版本 A：key-a 可用，上游收到 master A
	if st, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", "sk-key-a", chatBody); st != 200 {
		t.Fatalf("vA key-a status = %d", st)
	}
	if got := fu.auth(); got != "Bearer sk-MASTER-A" {
		t.Errorf("vA 上游收到 %q", got)
	}

	// 热更新到版本 B：换 key / master / 模型
	vB := hotConfig(up.URL, "sk-MASTER-B", "sk-key-b", "grok-test", "1.23")
	if err := os.WriteFile(path, []byte(vB), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// 旧 key 立即失效
	if st, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", "sk-key-a", chatBody); st != 401 {
		t.Errorf("热更新后旧 key status = %d, want 401", st)
	}
	// 新 key 可用，且上游收到新 master
	b2 := "{\"model\":\"grok-test\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
	if st, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", "sk-key-b", b2); st != 200 {
		t.Fatalf("vB key-b status = %d", st)
	}
	if got := fu.auth(); got != "Bearer sk-MASTER-B" {
		t.Errorf("vB 上游收到 %q", got)
	}
	// /models 反映新模型列表
	st, modelsBody, _ := doReq(t, srv, "GET", "/v1/models", "sk-key-b", "")
	if st != 200 {
		t.Fatalf("models status = %d", st)
	}
	if !strings.Contains(modelsBody, "grok-test") || strings.Contains(modelsBody, "mimo-v2.5") {
		t.Errorf("models 未反映热更新配置: %s", modelsBody)
	}
}

// ---- 智能提额（boost）测试 ----

func boostCfg() config.Boost {
	return config.Boost{
		Enabled: true, BaseOveragePercent: 105,
		TriggerPercent: 90, BoostPercent: 150,
		PoolMaxPercent: 85, OtherWindowMaxPercent: 95,
	}
}

// seedUnits 直接向 db 写入指定美元金额的消费记录（落在当前 5h 窗口内）。
func seedUnits(t *testing.T, db *store.Store, uuid string, usd float64) {
	t.Helper()
	seedUnitsAt(t, db, uuid, usd, time.Now())
}

// seedUnitsAt 在指定时间写入消费记录（用于把记录放在 5h 窗口外、1w 窗口内等）。
func seedUnitsAt(t *testing.T, db *store.Store, uuid string, usd float64, at time.Time) {
	t.Helper()
	if err := db.RecordUsage(uuid, "sk-seed", "deepseek-v4-flash", "/v1/chat/completions", meter.Usage{}, meter.USDToUnits(usd), at); err != nil {
		t.Fatal(err)
	}
}

// apiWindow 查询 /api/usage 并返回某个窗口信息。
func apiWindow(t *testing.T, srv *httptest.Server, key, period string) map[string]any {
	t.Helper()
	status, body, _ := doReq(t, srv, "POST", "/api/usage", "", `{"key":"`+key+`"}`)
	if status != 200 {
		t.Fatalf("/api/usage status = %d: %s", status, body)
	}
	var obj struct {
		Windows map[string]map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}
	return obj.Windows[period]
}

// 5h 限额 L=2.4。boost 未启用：硬卡 = L（严格）。
func TestBoostDisabledStrictLimit(t *testing.T) {
	fu := &fakeUpstream{zeroUsage: true}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil) // boost 默认 disabled
	srv := httptest.NewServer(p)
	defer srv.Close()

	seedUnits(t, db, "uuid-1", 2.4*0.99)
	if st, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("0.99L status = %d, want 200", st)
	}
	seedUnits(t, db, "uuid-1", 2.4*0.02) // 累计 1.01L
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 429 {
		t.Fatalf("1.01L status = %d: %s, want 429", st, body)
	}
}

// boost 启用但池子闸门挡住（总池 90 ≥ 85）：不提额，硬卡 = L×105%。
func TestBoost105BufferPoolGateBlocksBoost(t *testing.T) {
	snap := okSnapshot()
	snap.Rolling.Percent = 90 // 池子接近用尽，不应提额
	fu := &fakeUpstream{zeroUsage: true}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, &fixedBalance{snap: snap}, func(c *config.Config) { c.Boost = boostCfg() })
	srv := httptest.NewServer(p)
	defer srv.Close()

	seedUnits(t, db, "uuid-1", 2.4*1.04)
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("1.04L status = %d: %s, want 200（105%% 缓冲内）", st, body)
	}
	w := apiWindow(t, srv, testUser1, "5h")
	if w["boosted"] != false {
		t.Errorf("池子闸门应阻止提额, windows=%v", w)
	}
	seedUnits(t, db, "uuid-1", 2.4*0.02) // 累计 1.06L
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 429 {
		t.Fatalf("1.06L status = %d: %s, want 429（超出 105%% 硬卡）", st, body)
	}
}

// 条件满足：0.95L 触发提额 → 硬卡 1.5L；API 报告 boosted/boost_limit；前端字段完整。
func TestBoostTriggersAndRaisesLimit(t *testing.T) {
	fu := &fakeUpstream{zeroUsage: true}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, func(c *config.Config) { c.Boost = boostCfg() })
	srv := httptest.NewServer(p)
	defer srv.Close()

	seedUnits(t, db, "uuid-1", 2.4*0.95) // ≥ 90% 触发
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("0.95L status = %d: %s", st, body)
	}
	w := apiWindow(t, srv, testUser1, "5h")
	if w["boosted"] != true {
		t.Fatalf("应已提额, windows=%v", w)
	}
	if bl, _ := w["boost_limit"].(float64); bl != 3.6 {
		t.Errorf("boost_limit = %v, want 3.6", w["boost_limit"])
	}
	if lim, _ := w["limit"].(float64); lim != 2.4 {
		t.Errorf("limit 应保持原版 2.4, got %v", w["limit"])
	}
	if pct, _ := w["percent"].(float64); pct < 94 || pct > 96 {
		t.Errorf("percent 应按原限额显示 ≈95, got %v", w["percent"])
	}

	// 提额后用到 1.2L（<1.5L）放行；同周期不再重复提额（boost_limit 仍 3.6）
	seedUnits(t, db, "uuid-1", 2.4*0.25) // 累计 1.2L
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("1.2L status = %d: %s, want 200", st, body)
	}
	w2 := apiWindow(t, srv, testUser1, "5h")
	if bl, _ := w2["boost_limit"].(float64); bl != 3.6 {
		t.Errorf("同周期不应重复提额, boost_limit = %v", w2["boost_limit"])
	}

	// 1.6L > 1.5L → 429
	seedUnits(t, db, "uuid-1", 2.4*0.4) // 累计 1.6L
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 429 {
		t.Fatalf("1.6L status = %d: %s, want 429（超提额硬卡）", st, body)
	}
}

// 跨窗口健康检查：1w 已近限（≥95%）时，5h 不提额。
func TestBoostOtherWindowBlocks(t *testing.T) {
	fu := &fakeUpstream{zeroUsage: true}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, func(c *config.Config) { c.Boost = boostCfg() })
	srv := httptest.NewServer(p)
	defer srv.Close()

	// 1w 限额 6.0：5h 的 2.28 也计入 1w，因此 1w 只需额外 seed 3.42（合计 5.70 = 95%×6，
	// ≥95% 挡提额，且 <6.3 不拦请求）。记录放在 5h 窗口外（now-6h）。
	seedUnitsAt(t, db, "uuid-1", 3.42, time.Now().Add(-6*time.Hour))
	seedUnits(t, db, "uuid-1", 2.4*0.95) // 5h 到 95%，触发条件满足但跨窗口挡
	if st, body, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("status = %d: %s", st, body)
	}
	w := apiWindow(t, srv, testUser1, "5h")
	if w["boosted"] != false {
		t.Errorf("跨窗口不健康应阻止提额, windows=%v", w)
	}
}

// 重置（resetsAt 变化）后允许重新提额。
func TestBoostResetAllowsReBoost(t *testing.T) {
	snap := okSnapshot()
	fs := &fixedBalance{snap: snap}
	fu := &fakeUpstream{zeroUsage: true}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, db := newTestProxy(t, up.URL, fs, func(c *config.Config) { c.Boost = boostCfg() })
	srv := httptest.NewServer(p)
	defer srv.Close()

	seedUnits(t, db, "uuid-1", 2.4*0.95)
	if st, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("status = %d", st)
	}
	if w := apiWindow(t, srv, testUser1, "5h"); w["boosted"] != true {
		t.Fatal("第一周期应提额")
	}
	// 模拟 provider 重置：resetsAt 前移（新周期窗口起点回到 seed 之前，旧消费仍在窗口内）
	snap.Rolling.ResetsAt = snap.Rolling.ResetsAt.Add(-5 * time.Hour)
	if st, _, _ := doReq(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody); st != 200 {
		t.Fatalf("新周期 status = %d", st)
	}
	if w := apiWindow(t, srv, testUser1, "5h"); w["boosted"] != true {
		t.Fatal("新周期应允许重新提额（否则 boosted 应为 false）")
	}
}

// ---- 最小干预透传测试 ----

// TestForwardPassThroughHeaders 验证：除认证外，UA 与自定义头原样透传到上游，
// 且 /v1 前缀剥离、Authorization 替换为母 key。
func TestForwardPassThroughHeaders(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	h := map[string]string{
		"User-Agent":     "MyCustomSDK/1.2.3",
		"X-Test-Custom":  "hello-world",
		"X-API-Key":      "client-own-key-should-not-leak",
	}
	st, _, _ := doReqH(t, srv, "POST", "/v1/chat/completions", testUser1, chatBody, h)
	if st != 200 {
		t.Fatalf("status = %d", st)
	}
	if fu.auth() != "Bearer "+testMaster {
		t.Errorf("上游认证 = %q, want 母 key", fu.auth())
	}
	if fu.lastUA != "MyCustomSDK/1.2.3" {
		t.Errorf("UA 应透传客户端值, got %q", fu.lastUA)
	}
	if fu.lastXTest != "hello-world" {
		t.Errorf("自定义头应透传, got %q", fu.lastXTest)
	}
	if fu.lastPath != "/chat/completions" {
		t.Errorf("应剥离 /v1 前缀, got %q", fu.lastPath)
	}
	// 客户端自己的 X-API-Key 不得透传（认证全部替换为母 key）
	if fu.lastXTest == "client-own-key-should-not-leak" {
		t.Error("客户端 X-API-Key 不应透传")
	}
}

// TestForwardRequestBodyUnchanged 验证非流式请求体原样到达上游（字节完全一致）。
func TestForwardRequestBodyUnchanged(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`
	st, _, _ := doReqH(t, srv, "POST", "/v1/chat/completions", testUser1, body, nil)
	if st != 200 {
		t.Fatalf("status = %d", st)
	}
	if string(fu.lastBody) != body {
		t.Error("非流式请求体应逐字节原样透传")
	}
}

// TestForwardOversizeBody413 验证超大请求体返回 413，不静默截断转发。
func TestForwardOversizeBody413(t *testing.T) {
	fu := &fakeUpstream{}
	up := httptest.NewServer(fu.handler())
	defer up.Close()
	p, _ := newTestProxy(t, up.URL, &fixedBalance{snap: okSnapshot()}, nil)
	srv := httptest.NewServer(p)
	defer srv.Close()

	// 构造 Content-Length 超过 512MB 的请求（不真正发送大 body）
	big := "{\"model\":\"deepseek-v4-flash\",\"messages\":[]}" + strings.Repeat(" ", 513<<20)
	// 用 HTTP 客户端手动设置 ContentLength 后发送，避免构造 513MB 真实数据
	req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testUser1)
	req.ContentLength = int64(len(big))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body status = %d, want 413", resp.StatusCode)
	}
}