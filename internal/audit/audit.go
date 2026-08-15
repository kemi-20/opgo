package audit

import (
	"bytes"
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
	"time"

	"opgo/internal/balance"
	"opgo/internal/config"
	"opgo/internal/proxy"
	"opgo/internal/store"
)

// fixedBalance 已同步的假快照。
type fixedBalance struct{ snap *balance.Snapshot }

func (f *fixedBalance) Snapshot() (*balance.Snapshot, bool) {
	cp := *f.snap
	return &cp, true
}

// noBalance 从未同步（触发 503）。
type noBalance struct{}

func (n *noBalance) Snapshot() (*balance.Snapshot, bool) { return nil, false }

func fixedSnapshot() *balance.Snapshot {
	now := time.Now().UTC()
	return &balance.Snapshot{
		FetchedAt: now,
		Rolling:   balance.CapInfo{Status: "ok", Percent: 5, ResetsAt: now.Add(2 * time.Hour)},
		Weekly:    balance.CapInfo{Status: "ok", Percent: 0, ResetsAt: now.Add(24 * time.Hour)},
		Monthly:   balance.CapInfo{Status: "ok", Percent: 0, ResetsAt: now.Add(24 * time.Hour)},
	}
}

// Run 密钥防泄露自检：加载真实 config，用临时 sqlite + 本地假上游探测全部端点，
// 断言任何响应体/响应头都不含母 key 与任何子 key；假上游收到母 key 视为正向校验。
func Run(cfg *config.Config, indexHTML []byte, log *slog.Logger) error {
	var failed int
	check := func(name string, fn func() error) {
		if err := fn(); err != nil {
			failed++
			log.Error("[FAIL] "+name, "err", err)
		} else {
			log.Info("[PASS] " + name)
		}
	}

	keys := []string{cfg.MasterKey}
	for i := range cfg.Users {
		keys = append(keys, cfg.Users[i].Keys...)
	}
	assertNoKeys := func(name string, body []byte, hdr http.Header) error {
		for _, k := range keys {
			if bytes.Contains(body, []byte(k)) {
				return fmt.Errorf("%s 响应体包含 key", name)
			}
			for _, vs := range hdr {
				for _, v := range vs {
					if strings.Contains(v, k) {
						return fmt.Errorf("%s 响应头包含 key", name)
					}
				}
			}
		}
		return nil
	}

	var mu sync.Mutex
	var gotAuth string
	var gotPath string
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, "{\"id\":\"audit\",\"object\":\"chat.completion\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}")
	}))
	defer fakeUpstream.Close()

	cfgUp := *cfg
	cfgUp.UpstreamBase = fakeUpstream.URL

	db1Path := filepath.Join(os.TempDir(), fmt.Sprintf("opgo-audit-main-%d.db", time.Now().UnixNano()))
	db1, err := store.Open(db1Path)
	if err != nil {
		return err
	}
	defer db1.Close()
	defer os.Remove(db1Path)

	srv1 := httptest.NewServer(proxy.New(&cfgUp, db1, indexHTML, &fixedBalance{snap: fixedSnapshot()}, log))
	defer srv1.Close()
	srv2 := httptest.NewServer(proxy.New(&cfgUp, db1, indexHTML, &noBalance{}, log))
	defer srv2.Close()

	cfgTiny := cfgUp
	cfgTiny.LimitsPerUser = map[string]float64{"5h": 0.00000001, "1w": 0.00000001, "1m": 0.00000001}
	db2Path := filepath.Join(os.TempDir(), fmt.Sprintf("opgo-audit-tiny-%d.db", time.Now().UnixNano()))
	db2, err := store.Open(db2Path)
	if err != nil {
		return err
	}
	defer db2.Close()
	defer os.Remove(db2Path)
	srv3 := httptest.NewServer(proxy.New(&cfgTiny, db2, indexHTML, &fixedBalance{snap: fixedSnapshot()}, log))
	defer srv3.Close()

	cfgBad := cfgUp
	cfgBad.UpstreamBase = "http://127.0.0.1:1"
	srv4 := httptest.NewServer(proxy.New(&cfgBad, db2, indexHTML, &fixedBalance{snap: fixedSnapshot()}, log))
	defer srv4.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	userKey := cfg.Users[0].Keys[0]

	check("首页不含 key", func() error {
		status, body, hdr := get(client, srv1.URL+"/")
		if status != 200 {
			return fmt.Errorf("首页状态 %d", status)
		}
		return assertNoKeys("/", body, hdr)
	})
	check("healthz 不含 key", func() error {
		status, body, hdr := get(client, srv1.URL+"/healthz")
		if status != 200 {
			return fmt.Errorf("healthz 状态 %d", status)
		}
		return assertNoKeys("/healthz", body, hdr)
	})
	check("api/usage 正常 key 无泄露", func() error {
		status, body, hdr := post(client, srv1.URL+"/api/usage", map[string]any{"key": userKey})
		if status != 200 {
			return fmt.Errorf("api/usage 状态 %d: %s", status, body)
		}
		if err := assertNoKeys("/api/usage", body, hdr); err != nil {
			return err
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			return fmt.Errorf("api/usage 响应不是 JSON: %w", err)
		}
		if _, ok := obj["keys"]; ok {
			return fmt.Errorf("api/usage 响应包含 keys 字段")
		}
		if _, ok := obj["remark"]; ok {
			return fmt.Errorf("api/usage 响应包含 remark 字段")
		}
		return nil
	})
	check("api/usage 错误 key → 401 无泄露", func() error {
		status, body, hdr := post(client, srv1.URL+"/api/usage", map[string]any{"key": "sk-wrong-key"})
		if status != 401 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/api/usage-401", body, hdr)
	})
	check("api/admin 错误密码 → 401 无泄露", func() error {
		status, body, hdr := post(client, srv1.URL+"/api/admin", map[string]any{"password": "wrong"})
		if status != 401 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/api/admin-401", body, hdr)
	})
	check("api/admin 正确密码 → 无 keys 数组无泄露", func() error {
		status, body, hdr := post(client, srv1.URL+"/api/admin", map[string]any{"password": cfg.AdminPassword})
		if status != 200 {
			return fmt.Errorf("状态 %d: %s", status, body)
		}
		if err := assertNoKeys("/api/admin", body, hdr); err != nil {
			return err
		}
		var obj struct {
			Users []map[string]json.RawMessage `json:"users"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			return fmt.Errorf("api/admin 响应不是 JSON: %w", err)
		}
		for _, u := range obj.Users {
			if _, ok := u["keys"]; ok {
				return fmt.Errorf("api/admin 用户条目包含 keys 字段")
			}
		}
		return nil
	})
	check("models 无 key 泄露", func() error {
		status, body, hdr := req(client, http.MethodGet, srv1.URL+"/v1/models", "Bearer "+userKey, "")
		if status != 200 {
			return fmt.Errorf("状态 %d: %s", status, body)
		}
		return assertNoKeys("/v1/models", body, hdr)
	})
	check("v1/usage 官方格式无泄露", func() error {
		status, body, hdr := req(client, http.MethodGet, srv1.URL+"/v1/usage", "Bearer "+userKey, "")
		if status != 200 {
			return fmt.Errorf("状态 %d: %s", status, body)
		}
		if err := assertNoKeys("/v1/usage", body, hdr); err != nil {
			return err
		}
		var obj struct {
			Usage struct {
				Rolling map[string]any `json:"rolling"`
				Weekly  map[string]any `json:"weekly"`
				Monthly map[string]any `json:"monthly"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			return fmt.Errorf("/v1/usage 响应不是 JSON: %w", err)
		}
		for name, c := range map[string]map[string]any{"rolling": obj.Usage.Rolling, "weekly": obj.Usage.Weekly, "monthly": obj.Usage.Monthly} {
			if c == nil {
				return fmt.Errorf("/v1/usage 缺少 %s", name)
			}
			for _, f := range []string{"status", "percent", "resetsAt"} {
				if _, ok := c[f]; !ok {
					return fmt.Errorf("/v1/usage %s 缺少字段 %s", name, f)
				}
			}
		}
		return nil
	})
	check("v1/usage 未认证 → 401 无泄露", func() error {
		status, body, hdr := req(client, http.MethodGet, srv1.URL+"/v1/usage", "", "")
		if status != 401 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/v1/usage-401", body, hdr)
	})
	check("models 未认证 → 401 无泄露", func() error {
		status, body, hdr := req(client, http.MethodGet, srv1.URL+"/v1/models", "", "")
		if status != 401 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/v1/models-401", body, hdr)
	})
	check("转发后假上游收到母 key 且 /v1 前缀已剥离（仅上游方向）", func() error {
		mu.Lock()
		gotAuth = ""
		gotPath = ""
		mu.Unlock()
		status, body, hdr := req(client, http.MethodPost, srv1.URL+"/v1/chat/completions", "Bearer "+userKey,
			"{\"model\":\"deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
		if status != 200 {
			return fmt.Errorf("转发状态 %d: %s", status, body)
		}
		if err := assertNoKeys("/forward", body, hdr); err != nil {
			return err
		}
		mu.Lock()
		a := gotAuth
		pa := gotPath
		mu.Unlock()
		if a != "Bearer "+cfg.MasterKey {
			return fmt.Errorf("假上游收到 %q，期望 Bearer 母 key", a)
		}
		if pa != "/chat/completions" {
			return fmt.Errorf("假上游收到路径 %q，期望 /chat/completions（/v1 前缀应被剥离）", pa)
		}
		return nil
	})
	check("未知模型 → 403 无泄露", func() error {
		status, body, hdr := req(client, http.MethodPost, srv1.URL+"/v1/chat/completions", "Bearer "+userKey,
			"{\"model\":\"unknown-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
		if status != 403 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/403", body, hdr)
	})
	check("余额未同步 → 503 无泄露", func() error {
		status, body, hdr := req(client, http.MethodPost, srv2.URL+"/v1/chat/completions", "Bearer "+userKey,
			"{\"model\":\"deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
		if status != 503 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/503", body, hdr)
	})
	check("个人限额 → 429 无泄露", func() error {
		first, _, _ := req(client, http.MethodPost, srv3.URL+"/v1/chat/completions", "Bearer "+userKey,
			"{\"model\":\"deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
		if first != 200 {
			return fmt.Errorf("首次请求状态 %d", first)
		}
		status, body, hdr := req(client, http.MethodPost, srv3.URL+"/v1/chat/completions", "Bearer "+userKey,
			"{\"model\":\"deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
		if status != 429 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/429", body, hdr)
	})
	check("上游不可达 → 502 无泄露", func() error {
		status, body, hdr := req(client, http.MethodPost, srv4.URL+"/v1/chat/completions", "Bearer "+userKey,
			"{\"model\":\"deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
		if status != 502 {
			return fmt.Errorf("状态 %d", status)
		}
		return assertNoKeys("/502", body, hdr)
	})

	if failed > 0 {
		return fmt.Errorf("%d 项检查未通过", failed)
	}
	return nil
}

func get(client *http.Client, url string) (int, []byte, http.Header) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, []byte(err.Error()), nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func post(client *http.Client, url string, v any) (int, []byte, http.Header) {
	b, _ := json.Marshal(v)
	return req(client, http.MethodPost, url, "", string(b))
}

func req(client *http.Client, method, url, auth, body string) (int, []byte, http.Header) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, []byte(err.Error()), nil
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte(err.Error()), nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}
