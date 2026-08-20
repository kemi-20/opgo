# opgo

opgo 是一个把 Coding Plan 套餐共享给多人的轻量网关：成员请求透明转发到上游，按模型单价与真实 token 用量计费（内部 8 位小数精度），额度按 5小时 / 一周 / 31天 窗口限制（窗口锚点来自上游实时余量接口），并提供 Web 用量查询页。

## 功能
- 透明反代：仅替换认证头，请求体原样转发
- Muse 原生搜索兼容：仅对 `muse-spark-1.2-contributor` 发往 Responses 的 `web_search` 工具强制删除上游不支持的 `search_content_types`；模型名、协议、工具类型或字段任一不匹配时逐字节不修改，且无需配置
- 按 token 精确计费：模型单价写在 config.jsonc（已预置 deepseek-v4-flash、deepseek-v4-pro、mimo-v2.5、gpt-5.6-luna、hy3、muse-spark-1.2-contributor 定价）
- 每人（uuid）独立额度：5小时 / 一周 / 31天 滚动窗口，多 key 共享
- 峰谷时计费：模型级 peak 配置，Peak 时段（UTC 01:00-04:00 与 06:00-10:00）自动加倍扣款，config 中只填谷时价格
- 模型 tag：pricing 中可配置 tag，Web 定价表模型名右侧显示
- 总池保护：以上游实时余量接口为准，额度用尽即 429
- 流式请求照常计费（Chat Completions 自动注入 include_usage；Responses / Anthropic 读取标准末尾 usage）
- SQLite 记录全部用量
- Web 查询页：普通用户查自己，管理员看全部用户与总池
- 密钥防泄露：任何接口/前端都不返回任何 key；内置 `opgo -audit` 自检
- 单文件静态二进制，Windows / Linux 均可运行

## 配置文件（.jsonc）

- 配置文件名称为 `config.jsonc`（JSONC = JSON with Comments）。程序会自动剥离注释再解析，因此**行内 `//` 注释可以放心写**；VSCode 打开时把语言模式选为 `JSONC` 即可消除红色波浪线（标准 JSON 校验器不允许注释）。
- 兼容旧版 `config.json`：若目录下只有 `.json` 则直接读取；两者同时存在时**优先读 `.jsonc`**（不会自动改名或覆盖旧文件）。
## 一键安装（Ubuntu / Debian amd64）

```bash
curl -fsSL https://raw.githubusercontent.com/kemi-20/opgo/main/install.sh | sudo bash
```

首次启动会自动生成 /opt/opgo/config.jsonc（示例配置），编辑后重启：

```bash
sudo systemctl restart opgo
```

## Windows 本地运行

安装 Go 后：

```bash
go build -o opgo.exe .
.\opgo.exe
```

首次运行会在当前目录自动生成 config.jsonc，修改后重启。

## 访问方式（客户端配置）

客户端把 baseURL 指向 `http://<主机IP>:3003/v1`：

- OpenAI SDK / openai-compatible：`baseURL: http://IP:3003/v1`，自动请求 `/v1/chat/completions`、`/v1/responses`
- Anthropic SDK：`baseURL: http://IP:3003/v1`，自动请求 `/v1/messages`（x-api-key 填你的用户 key）
- 模型列表：`GET http://IP:3003/v1/models`
- 套餐余量（与上游官方格式一致）：`GET http://IP:3003/v1/usage`（Authorization: Bearer 你的用户 key）

代理会自动剥离 `/v1` 前缀并转发到上游对应端点；`/v1/models` 与 `/v1/usage` 由本地提供（配置与实时快照），不经过上游。

## 配置说明

| 字段 | 说明 |
| --- | --- |
| listen | 监听地址，默认 :3003 |
| upstream_base | 上游地址（必填） |
| master_key | 母 key（必填） |
| admin_password | Web 管理员密码（必填） |
| balance_url | 余量接口，留空用默认值 |
| balance_interval_seconds | 余量同步间隔，默认 120 |
| rate_limit_per_minute | 每用户每分钟限流，0=不限 |
| limits_per_user | 每人的 5h/1w/1m 美元限额 |
| pricing | 模型单价（每百万 token，config 填谷时价）+ context_length + max_output_tokens + modality + transformation + tag（前端模型名右侧标签，可空） |
| boost | 智能提额（见下） |
| users | uuid + 备注（可空）+ key 列表 |

## 峰谷时（peak）

peak 配置在**模型内**（pricing 每个模型条目中），只有配置了 peak 的模型才启用峰谷计价。**config 中填写谷时价格**，峰时自动按倍率加倍扣款，无需额外填写峰时价格。

```json
"deepseek-v4-flash": {
  "input_per_million": 0.44, // 谷时价
  "output_per_million": 1.32,
  "cached_read_per_million": 0.014,
  "cached_write_per_million": 0,
  "peak": {
    "enabled": true, // 是否启用（配置了 peak 默认启用）
    "multiplier": 2, // 峰时倍率（默认 2）
    "windows": [["01:00", "04:00"], ["06:00", "10:00"]] // 峰时时段（UTC，含起不含止）
  }
}
```

- 峰时窗口：01:00-04:00 与 06:00-10:00（UTC），其他时间为谷时；可自行修改 windows。
- 未配置 `peak` 的模型始终按 config 价格计费（无峰谷）。
- 可 `"enabled": false` 单独关闭某模型的峰谷。
- 当前示例仅 DeepSeek 双模型配置了 peak：deepseek-v4-flash 谷时 $0.44/$1.32/$0.014，峰时自动按 $0.88/$2.64/$0.028 计费（官网 Off-Peak 价的 2 倍，峰时再翻倍）。

## 模型模态（modality）

每个模型可在 pricing 中配置 `modality` 字段描述输入/输出模态，格式为 `输入1+输入2->输出1`。留空默认为 `text->text`。

```json
"pricing": {
  "deepseek-v4-flash": {
    "input_per_million": 0.44,
    "output_per_million": 1.32,
    "cached_read_per_million": 0.014,
    "cached_write_per_million": 0,
    "context_length": 1000000,
    "max_output_tokens": 384000,
    "modality": "text->text",
    "tag": "高峰期2x消耗",
    "peak": {
      "enabled": true,
      "multiplier": 2,
      "windows": [["01:00", "04:00"], ["06:00", "10:00"]]
    }
  },
  "mimo-v2.5": {
    "input_per_million": 0.14,
    "output_per_million": 0.28,
    "cached_read_per_million": 0.0028,
    "cached_write_per_million": 0,
    "context_length": 1000000,
    "max_output_tokens": 128000,
    "modality": "text+image+audio+video->text"
  }
}
```

示例配置预置：

| 模型 | modality |
|---|---|
| deepseek-v4-flash / deepseek-v4-pro | `text->text`（默认） |
| mimo-v2.5 | `text+image+audio+video->text` |
| gpt-5.6-luna | `text+image->text` |
| hy3 | `text->text`（纯文本） |
| muse-spark-1.2-contributor | `text+image+audio->text` |

`GET /v1/models` 会把 modality 自动拆为三个字段返回：

```json
{
  "id": "mimo-v2.5",
  "object": "model",
  "architecture": {
    "modality": "text+image+audio+video->text",
    "input_modalities": ["text", "image", "audio", "video"],
    "output_modalities": ["text"]
  }
}
```

## 协议转换（transformation）

每个模型可在 pricing 中配置 `transformation` 字段，把客户端任意格式自动转换为指定格式转发给上游，支持 OpenAI chat/completions、OpenAI Responses、Anthropic messages 三种格式，完整覆盖**非流式 / 流式 / 思考内容**互转。

```json
"pricing": {
  "mimo-v2.5": {
    "input_per_million": 0.14,
    "output_per_million": 0.28,
    "cached_read_per_million": 0.0028,
    "cached_write_per_million": 0,
    "context_length": 1000000,
    "max_output_tokens": 128000,
    "transformation": "openai_completions"
  }
}
```

- **留空 / 不填 / `false` / `0`**：不转换，透传只替换认证头（默认行为，与以前完全一致）
- **`openai_completions`**：客户端无论用 chat/completions、Responses 还是 Anthropic 访问，都转换为 chat/completions 发给上游
- **`openai_responses`**：统一转换为 Responses（`/responses`）发给上游
- **`anthropic`**：统一转换为 Anthropic messages（`/messages`）发给上游

示例：客户端用 Anthropic SDK（`/v1/messages`）访问，模型配置 `transformation: "openai_completions"`，opgo 会把请求转为 chat/completions 发往上游，再把上游响应转回 Anthropic 格式返回——流式、思考（reasoning_content ↔ thinking）都自动处理。计费仍按上游返回的真实 token 用量进行，不受转换影响。

**模型适配建议**：上游对部分模型只完整支持一种端点。示例配置的默认策略：

| 模型 | transformation | 原因 |
|---|---|---|
| deepseek-v4-flash / deepseek-v4-pro | （留空透传） | 上游原生端点完整，直接透传只替换认证 |
| mimo-v2.5 | `openai_completions` | 上游只有 chat/completions 完整（Responses 不稳定、messages 无思考），统一转 completions |
| hy3 | `openai_completions` | 统一转 chat/completions 发上游（与 mimo 同策略） |
| gpt-5.6-luna | `openai_responses` | 上游原生支持 Responses（含 web_search），统一转 responses |
| muse-spark-1.2-contributor | （留空透传） | 上游官方规范端点为 Responses（/v1/responses），原生三端点路由均在；Contributor 模型需在 Coding Plan workspace 显式开启数据收集 opt-in，未开启时上游返回 403 DataPolicyError |

客户端无论用哪种格式访问，opgo 都会自动转换为对应格式转发，保证思考/缓存/usage/工具完整。可按需覆盖各模型的 `transformation`（留空 = 透传）。

## 配置热更新

程序后台每 1 秒轮询 config 文件，修改保存后**无需重启**立即生效：

- users（增删用户/key）、pricing（价格/模型列表/context_length/max_output_tokens）、limits_per_user、master_key、admin_password、rate_limit_per_minute、upstream_base、balance_url、balance_interval_seconds
- 配置文件非法（JSON 错误或校验不通过）时自动保留旧配置并在日志告警，不影响运行
- 运行中配置文件被删除或暂时不可读取时会告警并保留最后有效配置；文件恢复后自动重新校验载入（不会用 example 覆盖原配置）
- 仅 `listen` 变更需要重启生效，检测到时会打印警告日志

## 智能提额（boost，可选）

`boost.enabled` 开启后：

- **后台缓冲**：正常情况下限硬卡 = 限额 × 105%（前端仍按原限额显示百分比），防止对话写到一半被 429 中断
- **智能提额**：某窗口用量达到限额的 90% 时，若同时满足「该窗口总池余量充足（percent < pool_max_percent 且 status=ok）」与「另外两个窗口用量 < other_window_max_percent」，自动把该窗口限额提到 boost_percent%（默认 150%）
- 提额后硬卡 = 提额后限额（105% 不叠加）；**可多次提额**：每次逻辑相同——用量再次达到 当前档限额×90% 且池子/跨窗口健康时，再提到 当前档限额×150%（第 1 次 = 150%×L，第 2 次 = 225%×L，第 3 次 = 337.5%×L …）；max_boost_times 可限制每个重置周期最多提额次数（0 = 不限）；重置后自动恢复从头开始
- 前端：提额窗口在原始限额旁显示绿色徽章「智能提额至 $X.XX」，百分比永远按 config 原版限额计算；未提额且用量超过原限额（100%~105% 缓冲内）时显示橙色「已超额」徽章
- 提额状态仅存内存；总池 percent ≥ 100 / status 非 ok 的 429 硬拦截不受任何提额影响

## 计费与限额

- 每笔请求按响应中的真实 token 用量 × 单价计费，内部以 1e-8 美元为最小单位累加
- 窗口 = [重置时间 − 周期, 重置时间)，重置时间来自上游余量接口，三个额度各自独立
- 三种生成接口必须提供有效的顶层 `model`；缺失、类型错误或非法 JSON 会在本地返回 400，不会转发上游
- 任一有效生成请求开始前：总池用尽 / 个人窗口超限 / 超频 → 429

## Web 查询

打开 http://主机:3003/ ：

- 用户页签：输入自己的 key，查看三个窗口已用/限额/百分比与总池实时余量
- 管理员页签：输入密码，查看全部用户（uuid/备注/用量）与总池

## 密钥自检

```bash
opgo -audit -config /opt/opgo/config.jsonc
```

全部 PASS 退出码为 0；任何响应中出现 key 都会 FAIL。

## 数据查询

```bash
sqlite3 /opt/opgo/usage.db "select uuid, model, total_tokens, cached_write_tokens, reasoning_tokens, cost_units, datetime(created_at_epoch_ms/1000,'unixepoch') from usage order by id desc limit 20;"
```

## 发布

```bash
git tag v0.1.0 && git push origin v0.1.0
```

CI 会自动构建并发布 Release（opgo-linux-amd64 / opgo-windows-amd64.exe）。

## 注意

- 共享订阅可能违反上游服务条款，请自行确认
- 管理员密码走明文 HTTP，公网使用请在前置 Nginx/Caddy 加 HTTPS
- 同一窗口内并发请求可能把消费略推过限额（最多一个请求的量）
