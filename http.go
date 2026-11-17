// http.go: the HTTP transport.
//
// Primary: a hand-rolled HTTP/1.1 server over a raw TCP listener (minimal
// allocation, keep-alive, Content-Length + chunked request bodies, 50
// concurrent connections). Handlers are transport-agnostic: they take a
// Request and a ResponseWriter, so the same handlers run on any transport.
//
// Routing is method + path patterns; a segment is a literal, "{name}", a
// "{name}.git" suffixed capture, or a "{name...}" rest capture. Unknown
// routes answer 404 JSON, never HTML. Every REST reply carries a
// non-empty X-GitHub-Request-Id ("req-<n>", a deterministic counter).
//
// Auth: Authorization: Bearer <MAKEFUN_SHOP_TOKEN>. Missing/wrong -> 401
// JSON, on every route except GET /healthz (the env's boot probe).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is one HTTP request, transport-agnostic.
type Request struct {
	Method   string
	Path     string // decoded, no query
	RawQuery string
	Headers  map[string]string // lower-cased keys
	Body     []byte
	Params   map[string]string // path captures
	User     string            // viewer login, when authenticated
}

// ResponseWriter buffers one response; the server serializes it with an
// explicit Content-Length.
type ResponseWriter struct {
	status  int
	headers map[string]string
	body    []byte
	wrote   bool
}

func newResponseWriter() *ResponseWriter {
	return &ResponseWriter{status: 200, headers: make(map[string]string)}
}

// Status sets the response status code.
func (w *ResponseWriter) Status(code int) { w.status = code }

// Header sets a response header (case-insensitive key, stored lower-cased).
func (w *ResponseWriter) Header(k, v string) { w.headers[strings.ToLower(k)] = v }

// Write appends to the response body.
func (w *ResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	w.wrote = true
	return len(p), nil
}

// JSON writes v as a JSON body with the given status.
func JSON(w *ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		JSONError(w, 500, "internal error")
		return
	}
	JSONBytes(w, status, b)
}

// JSONBytes writes pre-serialized JSON with the given status.
func JSONBytes(w *ResponseWriter, status int, b []byte) {
	w.Header("Content-Type", "application/json")
	w.Status(status)
	w.Write(b)
}

// JSONError writes {"message": msg} with the given status.
func JSONError(w *ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"message": msg})
}

// DecodeJSON decodes the request body as JSON into v.
func DecodeJSON(r *Request, v any) error {
	if len(r.Body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(r.Body, v)
}

// Handler is a transport-agnostic request handler.
type Handler func(w *ResponseWriter, r *Request)

type route struct {
	method   string
	segments []string
	handler  Handler
}

// Router dispatches method + path patterns to handlers.
type Router struct {
	Store  *Store
	Token  string // expected bearer token; "" disables auth
	Viewer string // viewer login, for handlers

	mu     sync.RWMutex
	routes []route
	reqSeq atomic.Int64
}

func NewRouter(store *Store) *Router {
	return &Router{Store: store}
}

// Handle registers a handler for a method + path pattern.
func (rt *Router) Handle(method, pattern string, h Handler) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.routes = append(rt.routes, route{
		method:   method,
		segments: splitPath(pattern),
		handler:  h,
	})
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matchSegments matches path segments against a pattern, filling params.
// A "{name...}" segment captures the rest of the path.
func matchSegments(pattern, path []string, params map[string]string) bool {
	for i, seg := range pattern {
		if strings.HasSuffix(seg, "...}") && strings.HasPrefix(seg, "{") {
			name := seg[1 : len(seg)-4]
			params[name] = strings.Join(path[i:], "/")
			return true
		}
		if i >= len(path) {
			return false
		}
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			name := seg[1 : len(seg)-1]
			params[name] = path[i]
			continue
		}
		if strings.HasSuffix(seg, ".git") && strings.HasPrefix(seg, "{") {
			// "{name}.git": capture the segment minus the .git suffix.
			name := seg[1 : len(seg)-5]
			if !strings.HasSuffix(path[i], ".git") {
				return false
			}
			params[name] = strings.TrimSuffix(path[i], ".git")
			continue
		}
		if seg != path[i] {
			return false
		}
	}
	return len(path) == len(pattern)
}

// ServeRequest dispatches one request through auth + routing.
func (rt *Router) ServeRequest(w *ResponseWriter, r *Request) {
	if r.Method == "GET" && r.Path == "/healthz" {
		JSON(w, 200, map[string]bool{"ok": true})
		return
	}
	// Auth on everything else.
	if rt.Token != "" {
		if r.Headers["authorization"] != "Bearer "+rt.Token {
			rt.annotate(w, r)
			JSONError(w, 401, "Bad credentials")
			return
		}
		r.User = rt.Viewer
	}
	rt.annotate(w, r)
	rt.mu.RLock()
	routes := rt.routes
	rt.mu.RUnlock()
	pathSegs := splitPath(r.Path)
	for _, rt2 := range routes {
		if rt2.method != r.Method {
			continue
		}
		params := make(map[string]string)
		if matchSegments(rt2.segments, pathSegs, params) {
			r.Params = params
			rt2.handler(w, r)
			return
		}
	}
	JSONError(w, 404, "Not Found")
}

// annotate stamps the per-request headers every REST reply carries.
func (rt *Router) annotate(w *ResponseWriter, r *Request) {
	if strings.HasPrefix(r.Path, "/api/") || r.Path == "/mcp" {
		n := rt.reqSeq.Add(1)
		w.Header("X-GitHub-Request-Id", "req-"+strconv.FormatInt(n, 10))
	}
}

// ---------------------------------------------------------------------------
// hand-rolled HTTP/1.1 server
// ---------------------------------------------------------------------------

// Server serves a Router over a raw TCP listener.
type Server struct {
	Router   *Router
	MaxConns int // 0 means 50
}

var reasonPhrases = map[int]string{
	200: "OK", 201: "Created", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 413: "Content Too Large",
	422: "Unprocessable Entity", 500: "Internal Server Error",
}

func reason(code int) string {
	if s, ok := reasonPhrases[code]; ok {
		return s
	}
	return "Status"
}

var readerPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 8192) },
}

// MaxBody is the largest request body accepted (packs can be large).
const MaxBody = 64 << 20

// Serve accepts connections until the listener closes.
func (sv *Server) Serve(ln net.Listener) error {
	max := sv.MaxConns
	if max <= 0 {
		max = 50
	}
	sem := make(chan struct{}, max)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			sv.serveConn(conn)
		}()
	}
}

func (sv *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer readerPool.Put(br)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		req, keepAlive, err := readRequest(br)
		if err != nil {
			return
		}
		w := newResponseWriter()
		func() {
			defer func() {
				// A handler panic must not kill the connection loop or
				// the server: answer 500 and keep serving.
				if rec := recover(); rec != nil {
					w.status = 500
					w.headers = map[string]string{"content-type": "application/json"}
					w.body = []byte(`{"message":"internal error"}`)
				}
			}()
			sv.Router.ServeRequest(w, req)
		}()
		_ = conn.SetWriteDeadline(time.Now().Add(120 * time.Second))
		if err := writeResponse(conn, w, keepAlive); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// readRequest parses one request from the buffered connection.
func readRequest(br *bufio.Reader) (*Request, bool, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, false, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, errors.New("bad request line")
	}
	method, target, version := parts[0], parts[1], parts[2]
	path, query := target, ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		path, query = target[:i], target[i+1:]
	}
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	headers := make(map[string]string)
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		colon := strings.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(hline[:colon]))
		val := strings.TrimSpace(hline[colon+1:])
		if old, ok := headers[key]; ok {
			headers[key] = old + ", " + val
		} else {
			headers[key] = val
		}
	}
	keepAlive := true
	if version == "HTTP/1.0" {
		keepAlive = false
	}
	if c := strings.ToLower(headers["connection"]); c == "close" {
		keepAlive = false
	} else if c == "keep-alive" {
		keepAlive = true
	}
	var body []byte
	if strings.ToLower(headers["transfer-encoding"]) == "chunked" {
		body, err = readChunked(br)
		if err != nil {
			return nil, false, err
		}
	} else if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, false, errors.New("bad content-length")
		}
		if n > MaxBody {
			return nil, false, errors.New("body too large")
		}
		if n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, false, err
			}
		}
	}
	return &Request{
		Method: method, Path: path, RawQuery: query,
		Headers: headers, Body: body,
	}, keepAlive, nil
}

// readChunked reads a chunked request body.
func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(line, 16, 64)
		if err != nil || n < 0 {
			return nil, errors.New("bad chunk size")
		}
		if n == 0 {
			// Trailer headers until an empty line.
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
					return out, nil
				}
			}
		}
		if int64(len(out))+n > MaxBody {
			return nil, errors.New("body too large")
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// Trailing CRLF after the chunk.
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

// writeResponse serializes one response with an explicit Content-Length.
func writeResponse(conn net.Conn, w *ResponseWriter, keepAlive bool) error {
	var b strings.Builder
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(w.status))
	b.WriteString(" ")
	b.WriteString(reason(w.status))
	b.WriteString("\r\n")
	keys := make([]string, 0, len(w.headers)+2)
	for k := range w.headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(w.headers[k])
		b.WriteString("\r\n")
	}
	b.WriteString("content-length: ")
	b.WriteString(strconv.Itoa(len(w.body)))
	b.WriteString("\r\n")
	if keepAlive {
		b.WriteString("connection: keep-alive\r\n")
	} else {
		b.WriteString("connection: close\r\n")
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return err
	}
	if len(w.body) > 0 {
		if _, err := conn.Write(w.body); err != nil {
			return err
		}
	}
	return nil
}
