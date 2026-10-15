package downloader

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type progressWriter struct {
	w       io.Writer
	onWrite func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 && p.onWrite != nil {
		p.onWrite(int64(n))
	}
	return n, err
}

func copyHeaders(req *http.Request, h http.Header) {
	if h == nil {
		return
	}
	for k, vals := range h {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
}

func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "tls handshake timeout"),
		strings.Contains(s, "timeout awaiting response headers"),
		strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "connection reset"),
		strings.Contains(s, "connection refused"),
		strings.Contains(s, "unexpected eof"),
		strings.Contains(s, "broken pipe"),
		strings.Contains(s, "server closed idle connection"),
		strings.Contains(s, "temporary"):
		return true
	default:
		return false
	}
}

// doGET 对无 body 的上游请求做瞬时错误重试（TLS 握手超时等）
func (e *Engine) doGET(ctx context.Context, method, rawURL string, headers http.Header, attempts int) (*http.Response, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			backoff := time.Duration(i) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if e.log != nil {
				e.log.Warn("upstream retry",
					zap.String("method", method),
					zap.String("url", rawURL),
					zap.Int("attempt", i+1),
					zap.Error(last),
				)
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		copyHeaders(req, headers)
		req.Header.Del("Accept-Encoding")
		resp, err := e.client.Do(req)
		if err == nil {
			return resp, nil
		}
		last = err
		if !isTransientNetErr(err) {
			return nil, err
		}
	}
	return nil, last
}
