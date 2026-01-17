# right.codes 高性能反向代理（OpenAI Responses API Compatible）

这是一个专为 `right.codes`（OpenAI Compatible API，透传请求）设计的高性能反向代理中间件。

它主要做两件事：

1) **兼容旧/非标准客户端请求**：把顶层 `instructions` 字段清洗为标准的 `input` 消息数组，并注入为 **Developer Message**（提升提示词优先级）。  
2) **自动补齐 Prompt Caching Key**：当请求体缺少 `prompt_cache_key` 时，代理会自动生成一个稳定的 key 并写入请求体，用于提升 Prompt Caching 的路由/命中概率（不影响对话语义）。

> 重要说明  
> - 本代理**不自动维护** `previous_response_id`（最正确、最通用的做法）。客户端带了就透传，不带就不管。  
> - `prompt_cache_key` 只用于缓存路由优化，**不是**对话上下文/会话复用机制。

---

## ⚡ 核心特性

- **高性能反代**：面向高并发场景优化；非目标请求近乎零开销透传。
- **请求自动兼容**：将非标准顶层 `instructions` 转换为标准 `input` 数组中的 Developer Message。
- **自动补 prompt_cache_key**：请求体缺失该字段时自动补齐；key 为稳定派生值（不会直接泄露原始 API Key）。
- **Gzip 透明处理**：自动解压 Gzip 请求体，改写后重置请求体长度，确保下游兼容。
- **Multi-turn 安全处理**：当请求体包含 `previous_response_id` 时，代理**不会**再把 `instructions` 迁移进 `input`，避免在多轮链路里重复注入导致 token 膨胀（但 `previous_response_id` 本身始终透传）。

---

## 🧩 请求体转换示例

**原始请求（客户端发送）**

```json
{
  "model": "gpt-4.1",
  "instructions": "You are a helpful assistant.",
  "input": "How's the weather?"
}
```

**转换后请求（发往服务端）**

```json
{
  "model": "gpt-4.1",
  "prompt_cache_key": "auto-derived-stable-key",
  "input": [
    { "role": "developer", "content": "You are a helpful assistant." },
    { "role": "user", "content": "How's the weather?" }
  ]
}
```

> 注：如果客户端已经提供 `prompt_cache_key`，代理不会覆盖；只在缺失时补齐。

---

## 🚀 快速开始

### 1) 环境要求

- Go（建议使用较新版本）
- 可访问上游

本项目依赖 `sonic`（高性能 JSON/AST）：

```bash
go get github.com/bytedance/sonic@latest
```

### 2) 运行

单文件项目，直接运行：

```bash
go run main.go
```

或编译为二进制：

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o rc-proxy main.go
./rc-proxy
```

默认监听：`0.0.0.0:18080`

---

## 🔧 客户端配置

把你的 OpenAI Compatible 客户端的目标地址指向代理即可。

- **上游（示例）**：`https://right.codes/.../v1/responses`
- **代理（示例）**：`http://<VPS_IP>:18080/v1/responses`

> 请确保最终请求命中的是 `POST /v1/responses`（该代理只对该接口做兼容处理，其它请求会直接透传）。

---

## 🧠 prompt_cache_key 说明（自动补齐）

- 代理仅在请求体缺少 `prompt_cache_key` 时自动补齐。
- key 通过请求头中的鉴权信息派生（例如 `Authorization` / `x-api-key` 等），并做哈希截断，避免直接暴露原始 key。
- `prompt_cache_key` 用于提升 Prompt Caching 的命中/路由稳定性，**不等同于会话**，也不会自动帮你实现多轮上下文。

---

## 🔁 previous_response_id 说明（不自动做）

- 代理不会自动生成或维护 `previous_response_id`。
- 如果你的客户端支持多轮：请由客户端保存上一轮 `response.id` 并在下一轮请求中携带 `previous_response_id`。
- 代理检测到请求体存在 `previous_response_id` 时，会避免再次迁移 `instructions`，以免多轮链路重复注入。

---

## ⚙️ 可选配置

核心配置集中在 `main.go` 顶部常量：

- `TargetHost`：上游地址（默认 `https://right.codes`）
- `LocalPort`：本地监听端口（默认 `:18080`）

---

## 📄 License

MIT License