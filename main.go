// main.go: the entry <-> env lifecycle (from RULES.txt / starter main.ts).
//
//  1. Bind an ephemeral port (port 0; never 8080, the env owns it).
//  2. POST {"port": N} to $MAKEFUN_ENV_URL/register.
//  3. Poll GET $MAKEFUN_ENV_URL/done until {"done": true}, then exit 0.
//
// The forge itself is the Go binary; main.ts (Worker 2) only spawns it and
// waits. The lifecycle client here is hand-rolled over net.Dial (no
// net/http import: the binary budget and the design's do-not-import list).
// A program that exits, crashes, or never registers before the boot budget
// ends scores 0, so register retries forever and done is polled forever.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const requestTimeout = 30 * time.Second

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")

	store := NewStore()
	router := NewRouter(store)
	router.Token = token
	router.Viewer = ViewerLogin()
	RegisterGitRoutes(router, store)
	registerREST(router, store)
	registerGraphQL(router, store)
	registerMCP(router, store)

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[entry] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)

	server := &Server{Router: router, MaxConns: 500}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ln) }()

	// A signal ends the process cleanly (the launcher forwards signals).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		ln.Close()
		os.Exit(0)
	}()

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		ln.Close()
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	// A dead listener must fail fast: without this select, Serve's error
	// sat in a channel nobody read and a failed bind would hang in
	// waitForDone until the boot budget expired (score 0).
	doneCh := make(chan struct{})
	go func() {
		waitForDone(envURL)
		close(doneCh)
	}()
	select {
	case err := <-serveErr:
		fmt.Fprintln(os.Stderr, "[entry] server stopped:", err)
		os.Exit(1)
	case <-doneCh:
	}
	ln.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
}

// retryDelay is the wait after `attempt` failed requests: exponential from
// 250ms, capped at 2s (the starter's shape).
func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	d := 250 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 2*time.Second {
		return 2 * time.Second
	}
	return d
}

// register POSTs the bound port until the env accepts it. 200 is normal;
// 409 means a first register already won. 400/405 are contract errors and
// end the program; anything else is retried, since the env may still be
// starting.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		status, _, err := httpDo(envURL, "POST", "/register", body)
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d", status)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done": true}; it never
// returns before that.
func waitForDone(envURL string) {
	for attempt := 0; ; attempt++ {
		status, respBody, err := httpDo(envURL, "GET", "/done", nil)
		if err == nil && status == 200 {
			var parsed struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(respBody, &parsed) == nil && parsed.Done {
				return
			}
		}
		if err != nil {
			time.Sleep(retryDelay(attempt))
		} else {
			time.Sleep(1 * time.Second)
		}
	}
}

// httpDo performs one tiny HTTP/1.1 request against the env origin and
// returns the status code and body. It sends Connection: close and reads
// until EOF (or Content-Length, whichever ends first).
func httpDo(envURL, method, path string, body []byte) (int, []byte, error) {
	hostport := strings.TrimPrefix(envURL, "http://")
	hostport = strings.TrimPrefix(hostport, "https://")
	if i := strings.IndexByte(hostport, '/'); i >= 0 {
		hostport = hostport[:i]
	}
	conn, err := net.DialTimeout("tcp", hostport, requestTimeout)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))

	var req bytes.Buffer
	req.WriteString(method + " " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + hostport + "\r\n")
	if body != nil {
		req.WriteString("Content-Type: application/json\r\n")
		req.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	req.WriteString("Connection: close\r\n")
	req.WriteString("\r\n")
	req.Write(body)
	if _, err := conn.Write(req.Bytes()); err != nil {
		return 0, nil, err
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	fields := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(fields) < 2 {
		return 0, nil, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, nil, fmt.Errorf("bad status line %q", statusLine)
	}
	contentLength := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.IndexByte(line, ':'); i >= 0 {
			if strings.EqualFold(strings.TrimSpace(line[:i]), "Content-Length") {
				contentLength, _ = strconv.Atoi(strings.TrimSpace(line[i+1:]))
			}
		}
	}
	var respBody []byte
	if contentLength >= 0 {
		respBody = make([]byte, contentLength)
		if _, err := io.ReadFull(br, respBody); err != nil {
			return 0, nil, err
		}
	} else {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(br); err != nil {
			return 0, nil, err
		}
		respBody = buf.Bytes()
	}
	return status, respBody, nil
}
