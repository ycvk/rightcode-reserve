package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
)

const (
	TargetHost = "https://right.codes"
	LocalPort  = ":18080"

	preGrow        = 32 << 10
	maxKeepBufCap  = 1 << 20 // 1MB
	copyBufSize    = 32 << 10
	maxKeepCopyCap = 1 << 20
)

var (
	bufPool  = sync.Pool{New: func() any { b := new(bytes.Buffer); b.Grow(preGrow); return b }}
	copyPool = sync.Pool{New: func() any { return make([]byte, copyBufSize) }}

	gzipPool = sync.Pool{New: func() any { return (*gzip.Reader)(nil) }}

	// fast-path keys (need to confirm ':' after optional whitespace)
	kInstrKey       = []byte(`"instructions"`)
	kPromptCacheKey = []byte(`"prompt_cache_key"`)
	kPrevRespIDKey  = []byte(`"previous_response_id"`)

	// sonic encoder: avoid trailing '\n'
	sonicAPI = sonic.Config{NoEncoderNewline: true}.Froze()
)

type pooledBody struct {
	r *bytes.Reader
	b *bytes.Buffer
}

func (p *pooledBody) Read(x []byte) (int, error) { return p.r.Read(x) }
func (p *pooledBody) Close() error               { putBuf(p.b); return nil }

func putBuf(b *bytes.Buffer) {
	if b.Cap() > maxKeepBufCap {
		return
	}
	b.Reset()
	bufPool.Put(b)
}

func setBody(req *http.Request, b *bytes.Buffer) {
	bs := b.Bytes()
	req.Body = &pooledBody{r: bytes.NewReader(bs), b: b}
	req.ContentLength = int64(len(bs))
	req.Header.Set("Content-Length", strconv.Itoa(len(bs)))
	req.Header.Del("Transfer-Encoding")
	req.TransferEncoding = nil
}

type proxyBufPool struct{}

func (proxyBufPool) Get() []byte { return copyPool.Get().([]byte) }
func (proxyBufPool) Put(p []byte) {
	if cap(p) > maxKeepCopyCap || cap(p) < copyBufSize {
		return
	}
	copyPool.Put(p[:copyBufSize])
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// fast-path: find `"key"` and ensure next non-ws char is ':'
func hasJSONKey(bs []byte, key []byte) bool {
	for off := 0; ; {
		idx := bytes.Index(bs[off:], key)
		if idx < 0 {
			return false
		}
		pos := off + idx + len(key)
		for pos < len(bs) && isWS(bs[pos]) {
			pos++
		}
		if pos < len(bs) && bs[pos] == ':' {
			return true
		}
		off = off + idx + 1
	}
}

func bytesToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

func isResponsesPath(p string) bool {
	const suf = "/v1/responses"
	if len(p) < len(suf) {
		return false
	}
	return p[len(p)-len(suf):] == suf
}

func getGzipReader(r io.Reader) (*gzip.Reader, error) {
	if v := gzipPool.Get(); v != nil {
		zr := v.(*gzip.Reader)
		if zr != nil {
			if err := zr.Reset(r); err == nil {
				return zr, nil
			}
		}
	}
	return gzip.NewReader(r)
}

func putGzipReader(zr *gzip.Reader) {
	_ = zr.Close()
	gzipPool.Put(zr)
}

// Derive a stable prompt_cache_key without leaking the raw API key.
// Priority: Authorization > x-api-key > api-key > (RemoteAddr + UA)
func derivePromptCacheKey(req *http.Request) string {
	var s string
	if v := req.Header.Get("Authorization"); v != "" {
		s = v
	} else if v := req.Header.Get("x-api-key"); v != "" {
		s = v
	} else if v := req.Header.Get("api-key"); v != "" {
		s = v
	} else {
		s = req.RemoteAddr + "|" + req.Header.Get("User-Agent")
	}
	sum := sha256.Sum256([]byte(s))
	// 16 bytes -> 32 hex chars
	return hex.EncodeToString(sum[:16])
}

// Pure byte insertion for prompt_cache_key at the start of a JSON object.
// Requires: prompt_cache_key missing AND we don't need to rewrite instructions.
func injectPromptCacheKeyFast(bs []byte, key string) (*bytes.Buffer, bool) {
	// skip leading whitespace
	i := 0
	for i < len(bs) && isWS(bs[i]) {
		i++
	}
	if i >= len(bs) || bs[i] != '{' {
		return nil, false
	}

	out := bufPool.Get().(*bytes.Buffer)
	out.Reset()
	out.Grow(len(bs) + len(key) + 32)

	// write up to and including '{'
	out.Write(bs[:i+1])

	// write `"prompt_cache_key":"<key>"`
	out.WriteString(`"prompt_cache_key":"`)
	out.WriteString(key)
	out.WriteByte('"')

	// detect empty object: next non-ws after '{' is '}'
	j := i + 1
	for j < len(bs) && isWS(bs[j]) {
		j++
	}
	if j < len(bs) && bs[j] == '}' {
		out.Write(bs[j:])
		return out, true
	}

	// non-empty object
	out.WriteByte(',')
	out.Write(bs[i+1:])
	return out, true
}

func main() {
	tu, err := url.Parse(TargetHost)
	if err != nil {
		slog.Error("failed to parse target host", "error", err)
		os.Exit(1)
	}

	rp := httputil.NewSingleHostReverseProxy(tu)
	rp.BufferPool = proxyBufPool{}
	rp.FlushInterval = -1 // 立即刷新，SSE/流式响应必需

	rp.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   4096,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	// 自定义错误处理：客户端主动断开是正常行为，不记录为错误
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() != nil {
			// context canceled 或 deadline exceeded - 客户端已断开，静默处理
			return
		}
		slog.Error("proxy error", "error", err)
		w.WriteHeader(http.StatusBadGateway)
	}

	od := rp.Director
	rp.Director = func(r *http.Request) {
		od(r)
		r.Host = tu.Host

		if r.Method == http.MethodPost && isResponsesPath(r.URL.Path) {
			tweakBodySonic(r)
		}
	}

	slog.Info("proxy server starting", "local", LocalPort, "target", TargetHost)
	s := &http.Server{
		Addr:              LocalPort,
		Handler:           rp,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := s.ListenAndServe(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func tweakBodySonic(req *http.Request) {
	if req.Body == nil {
		return
	}

	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()

	// pre-grow based on Content-Length if available
	if req.ContentLength > 0 && req.ContentLength < maxKeepBufCap {
		b.Grow(int(req.ContentLength))
	}

	// read body (support gzip)
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := getGzipReader(req.Body)
		if err != nil {
			req.Body.Close()
			putBuf(b)
			return
		}
		_, err = b.ReadFrom(zr)
		putGzipReader(zr)
		req.Body.Close()
		if err != nil {
			putBuf(b)
			return
		}
		req.Header.Del("Content-Encoding")
	} else {
		_, err := b.ReadFrom(req.Body)
		req.Body.Close()
		if err != nil {
			putBuf(b)
			return
		}
	}

	bs := b.Bytes()

	// 记录请求的 model 和 reasoning_effort（使用 sonic.Get 快速提取，不完整解析）
	if model, _ := sonic.Get(bs, "model"); model.Valid() {
		modelStr, _ := model.String()
		effort := "-"
		if re, _ := sonic.Get(bs, "reasoning", "effort"); re.Valid() {
			effort, _ = re.String()
		}
		slog.Info("request", "model", modelStr, "reasoning_effort", effort)
	}

	needInstr := hasJSONKey(bs, kInstrKey)
	hasPrompt := hasJSONKey(bs, kPromptCacheKey)
	hasPrev := hasJSONKey(bs, kPrevRespIDKey)

	// auto补 prompt_cache_key（缺失才补）
	// instructions 迁移：当 previous_response_id 存在时不做（避免多轮重复注入膨胀）
	shouldRewriteInstr := needInstr && !hasPrev

	// Fast path: only need to inject prompt_cache_key; no instructions rewrite.
	if !shouldRewriteInstr && !hasPrompt {
		key := derivePromptCacheKey(req)
		if out, ok := injectPromptCacheKeyFast(bs, key); ok {
			putBuf(b)
			setBody(req, out)
			return
		}
		// fall through to AST if not a plain object
	}

	// If no changes needed at all, keep original body
	if !shouldRewriteInstr && hasPrompt {
		setBody(req, b)
		return
	}

	// AST path (sonic)
	src := bytesToString(bs)
	p := ast.NewParserObj(src)
	root, perr := p.Parse()
	if err := p.ExportError(perr); err != nil {
		setBody(req, b)
		return
	}

	// ensure prompt_cache_key
	if !hasPrompt {
		key := derivePromptCacheKey(req)
		pk := root.Get("prompt_cache_key")
		if pk == nil || !pk.Exists() || pk.TypeSafe() == ast.V_NULL {
			_, _ = root.Set("prompt_cache_key", ast.NewString(key))
		}
	}

	if shouldRewriteInstr {
		ins := root.Get("instructions")
		if ins != nil && ins.Exists() && ins.TypeSafe() == ast.V_STRING {
			content := *ins
			_, _ = root.Unset("instructions")

			// dev = {"role":"developer","content":<ins>}
			dev := ast.NewObject([]ast.Pair{
				ast.NewPair("role", ast.NewString("developer")),
				ast.NewPair("content", content),
			})

			in := root.Get("input")

			// missing / null
			if in == nil || !in.Exists() || in.TypeSafe() == ast.V_NULL {
				_, _ = root.Set("input", ast.NewArray([]ast.Node{dev}))
				goto ENCODE
			}

			switch in.TypeSafe() {
			case ast.V_STRING:
				user := ast.NewObject([]ast.Pair{
					ast.NewPair("role", ast.NewString("user")),
					ast.NewPair("content", *in),
				})
				_, _ = root.Set("input", ast.NewArray([]ast.Node{dev, user}))

			case ast.V_ARRAY:
				// in-place prepend: Add at end then Move to 0
				if err := in.Add(dev); err == nil {
					if n, err := in.Len(); err == nil && n > 1 {
						_ = in.Move(0, n-1)
					}
				} else {
					_, _ = root.Set("input", ast.NewArray([]ast.Node{dev}))
				}

			default:
				_, _ = root.Set("input", ast.NewArray([]ast.Node{dev}))
			}
		}
	}

ENCODE:
	out := bufPool.Get().(*bytes.Buffer)
	out.Reset()
	out.Grow(len(bs) + 64)

	enc := sonicAPI.NewEncoder(out)
	if err := enc.Encode(&root); err != nil {
		putBuf(out)
		setBody(req, b)
		return
	}

	// Only now safe to return b (AST may reference src backed by b)
	putBuf(b)
	setBody(req, out)
}
