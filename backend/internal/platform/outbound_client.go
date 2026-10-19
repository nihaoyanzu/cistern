package platform

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewOutboundClient 构造遵守 Cistern 出站代理的 HTTP 客户端。
// proxyURL 为空时显式直连（不回退环境变量 HTTP(S)_PROXY）。
func NewOutboundClient(proxyURL string, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	tr.TLSHandshakeTimeout = timeout
	tr.ResponseHeaderTimeout = timeout
	tr.IdleConnTimeout = timeout
	tr.DialContext = (&net.Dialer{Timeout: timeout}).DialContext
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		tr.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	} else if u, err := url.Parse(proxyURL); err == nil && u.Scheme != "" && u.Host != "" {
		tr.Proxy = http.ProxyURL(u)
	} else {
		tr.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}
