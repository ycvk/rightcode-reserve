这是一个专为 `right.codes` (OpenAI Compatible API) 设计的高性能反向代理中间件。

它主要用于解决客户端 API 请求格式与服务端最新标准不兼容的问题。通过拦截 HTTP 请求，将非标准的顶层 `instructions` 字段“清洗”并注入到标准的 `input` 消息数组中，同时作为 Developer Message 提升 Prompt 优先级。

## ⚡ 核心特性

* **高性能架构**：基于 `net/http/httputil`，针对高并发场景深度优化。
* **极低 GC 压力**：全链路使用 `sync.Pool` 复用 `bytes.Buffer`、`map` 和 `[]byte`，极大减少内存分配。
* **智能 Fast-Path**：在进行昂贵的 JSON Unmarshal 之前，先通过字节扫描（Byte Scanning）预判是否存在目标字段，非目标请求几乎零延迟透传。
* **Gzip 透明处理**：自动检测并解压 Gzip 请求体，修改后重置 Content-Length，确保下游服务兼容性。
* **鲁棒的 JSON 处理**：完美兼容 `input` 字段的多态性（支持 String 简写模式或 Object Array 标准模式），并修复松散的 JSON 格式。

## 🛠 工作原理

该服务监听本地端口（默认 `:18080`），拦截 POST 请求路径包含 `/v1/responses` 的流量。

### 请求体转换示例

**原始请求 (客户端发送):**

> 客户端仅提供简单的 instructions 字段和字符串类型的 input。

```json
{
  "model": "gpt-4",
  "instructions": "You are a helpful assistant.",
  "input": "How's the weather?"
}

```

**转换后请求 (发往服务端):**

> 代理服务将 instructions 转换为 developer 角色，并标准化 input 数组。

```json
{
  "model": "gpt-4",
  "input": [
    {
      "role": "developer",
      "content": "You are a helpful assistant."
    },
    {
      "role": "user",
      "content": "How's the weather?"
    }
  ]
}

```

## 🚀 快速开始

### 1. 环境要求

* Go 1.18+

### 2. 运行

由于代码为单文件设计，直接运行即可：

```bash
go run main.go
```

或者编译为二进制文件：

```bash
go build -ldflags="-s -w" -o codex-proxy main.go
./codex-proxy

```

服务启动后将监听：`127.0.0.1:18080`

### 3. 客户端配置

请将你的客户端的 OpenAI 目标 URL 修改为本地代理地址。

* **原 URL**: `https://right.codes/codex/v1/responses`
* **新 URL**: `http://127.0.0.1:18080/codex/v1/responses`

> **注意**: 代理会自动处理 HTTPS 上游连接 (`TargetHost = "https://right.codes"`), 客户端只需通过 HTTP 连接本地代理。

## ⚙️ 配置调整

目前核心配置以常量形式硬编码在 `main.go` 顶部，可按需修改：

```go
const (
    TargetHost = "https://right.codes" // 上游目标地址
    LocalPort  = ":18080"              // 本地监听端口
    
    // 内存池参数 (一般无需修改)
    preGrow        = 32 << 10 // 32KB
    maxKeepBufCap  = 1 << 20  // 1MB
)

```

## 🧠 技术细节

本项目采用了多项 Golang 性能优化技巧：

1. **Memory Pooling**:
* `bufPool`: 复用 `bytes.Buffer` 用于请求体读写。
* `mapPool`: 复用 `map[string]json.RawMessage` 避免 JSON 解析时的 Map 分配。
* `proxyBufPool`: 注入 `httputil.ReverseProxy`，实现反代过程中的零拷贝转发。


2. **Byte-level Scanning**:
* 使用 `bytes.Index` 实现 `hasInstrKey` 函数。只有当字节流中确切包含 `"instructions"` 且后续紧跟冒号时，才触发 JSON 解析逻辑。


3. **JSON Splicing**:
* 在重组 `input` 数组时，直接操作 `[]byte` 切片进行拼接，而非构建完整的结构体对象图，最大限度保留原始数据特征并提升速度。


4. **Connection Keep-Alive**:
* 定制 `http.Transport`，开启 `MaxIdleConns` 和 `ForceAttemptHTTP2`，复用与上游的长连接。



## 📄 License

MIT License