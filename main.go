package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
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
	mapPool  = sync.Pool{New: func() any { return make(map[string]json.RawMessage, 32) }}
	copyPool = sync.Pool{New: func() any { return make([]byte, copyBufSize) }}

	// 更“收紧”的 fast-path：匹配 `"instructions"` 后还要确认后面（可带空白）紧跟 `:`
	kInstrKey = []byte(`"instructions"`)
)

func putBuf(b *bytes.Buffer) {
	if b.Cap() > maxKeepBufCap {
		return
	}
	b.Reset()
	bufPool.Put(b)
}

func putMap(m map[string]json.RawMessage) {
	for k := range m {
		delete(m, k)
	}
	mapPool.Put(m)
}

type pooledBody struct {
	r *bytes.Reader
	b *bytes.Buffer
}

func (p *pooledBody) Read(x []byte) (int, error) { return p.r.Read(x) }
func (p *pooledBody) Close() error               { putBuf(p.b); return nil }

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

// fast-path: 查找 `"instructions"` 并确认后面（可跳过空白）是 `:`
func hasInstrKey(bs []byte) bool {
	for off := 0; ; {
		idx := bytes.Index(bs[off:], kInstrKey)
		if idx < 0 {
			return false
		}
		pos := off + idx + len(kInstrKey)
		for pos < len(bs) && isWS(bs[pos]) {
			pos++
		}
		if pos < len(bs) && bs[pos] == ':' {
			return true
		}
		off = off + idx + 1
	}
}

// in[0]=='[' 前提下，判断是否是“只有空白的空数组”：[ ] / [\n\t ]
func isEmptyJSONArrayWS(in []byte) bool {
	for i := 1; i < len(in); i++ {
		if isWS(in[i]) {
			continue
		}
		return in[i] == ']'
	}
	return false
}

func main() {
	tu, err := url.Parse(TargetHost)
	if err != nil {
		log.Fatal(err)
	}

	rp := httputil.NewSingleHostReverseProxy(tu)
	rp.BufferPool = proxyBufPool{}

	rp.Transport = &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	od := rp.Director
	rp.Director = func(r *http.Request) {
		od(r)
		r.Host = tu.Host
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/v1/responses") {
			tweak(r)
		}
	}

	log.Printf("Proxy server starting on %s -> %s", LocalPort, TargetHost)
	s := &http.Server{Addr: LocalPort, Handler: rp, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(s.ListenAndServe())
}

func tweak(req *http.Request) {
	if req.Body == nil {
		return
	}

	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()

	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			req.Body.Close()
			putBuf(b)
			return
		}
		_, err = b.ReadFrom(zr)
		zr.Close()
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
	if !hasInstrKey(bs) {
		setBody(req, b)
		return
	}

	m := mapPool.Get().(map[string]json.RawMessage)
	if err := json.Unmarshal(bs, &m); err != nil {
		putMap(m)
		setBody(req, b)
		return
	}

	ins, ok := m["instructions"]
	if !ok || len(ins) <= 2 || ins[0] != '"' {
		putMap(m)
		setBody(req, b)
		return
	}
	delete(m, "instructions")

	in := m["input"]

	// dev = {"role":"developer","content":<ins>}
	dev := make([]byte, 0, 32+len(ins))
	dev = append(dev, `{"role":"developer","content":`...)
	dev = append(dev, ins...)
	dev = append(dev, '}')

	var newIn []byte
	if len(in) == 0 || (len(in) == 4 && in[0] == 'n') { // missing or null
		newIn = make([]byte, 0, len(dev)+2)
		newIn = append(newIn, '[')
		newIn = append(newIn, dev...)
		newIn = append(newIn, ']')
	} else {
		switch in[0] {
		case '"': // JSON string
			um := make([]byte, 0, 24+len(in))
			um = append(um, `{"role":"user","content":`...)
			um = append(um, in...)
			um = append(um, '}')

			newIn = make([]byte, 0, len(dev)+len(um)+3)
			newIn = append(newIn, '[')
			newIn = append(newIn, dev...)
			newIn = append(newIn, ',')
			newIn = append(newIn, um...)
			newIn = append(newIn, ']')

		case '[': // JSON array（修复空白空数组）
			if (len(in) == 2 && in[1] == ']') || isEmptyJSONArrayWS(in) {
				newIn = make([]byte, 0, len(dev)+2)
				newIn = append(newIn, '[')
				newIn = append(newIn, dev...)
				newIn = append(newIn, ']')
			} else {
				newIn = make([]byte, 0, len(dev)+len(in)+1)
				newIn = append(newIn, '[')
				newIn = append(newIn, dev...)
				newIn = append(newIn, ',')
				newIn = append(newIn, in[1:]...) // drop leading '['
			}

		default:
			newIn = make([]byte, 0, len(dev)+2)
			newIn = append(newIn, '[')
			newIn = append(newIn, dev...)
			newIn = append(newIn, ']')
		}
	}

	m["input"] = newIn

	out := bufPool.Get().(*bytes.Buffer)
	out.Reset()
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		putBuf(out)
		putMap(m)
		setBody(req, b)
		return
	}
	if n := out.Len(); n > 0 && out.Bytes()[n-1] == '\n' {
		out.Truncate(n - 1)
	}

	putMap(m)
	putBuf(b)

	setBody(req, out)
}