package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeGET 有限读取 body 的 GET 探测。
func ProbeGET(ctx context.Context, client *http.Client, rawURL string, maxBody int) (int, []byte, error) {
	return ProbeGETHeaders(ctx, client, rawURL, maxBody, nil)
}

// ProbeGETHeaders 带自定义头的有限 GET。
func ProbeGETHeaders(ctx context.Context, client *http.Client, rawURL string, maxBody int, extra http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "cistern-access-test/1.0")
	req.Header.Set("Accept", "*/*")
	copyExtraHeaders(req.Header, extra)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if maxBody <= 0 {
		maxBody = 64 << 10
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// ProbeHEAD HEAD 探测。
func ProbeHEAD(ctx context.Context, client *http.Client, rawURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "cistern-access-test/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, nil
}

// ProbeRangeGET 带 Range 的小 GET（部分源不支持 HEAD）。
func ProbeRangeGET(ctx context.Context, client *http.Client, rawURL string, start, end int) (int, []byte, error) {
	return ProbeRangeGETHeaders(ctx, client, rawURL, start, end, nil)
}

// ProbeRangeGETHeaders 带自定义头的 Range GET。
func ProbeRangeGETHeaders(ctx context.Context, client *http.Client, rawURL string, start, end int, extra http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "cistern-access-test/1.0")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	copyExtraHeaders(req.Header, extra)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// ProbeDownloadSample 拉取制品前缀字节；200/206 且读到内容才算下载通路可用。
func ProbeDownloadSample(ctx context.Context, client *http.Client, name, rawURL string, extra http.Header) AccessProbeCheck {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || rawURL == "/" {
		return AccessProbeCheck{Name: name, OK: false, Detail: "下载探测 URL 为空"}
	}
	const sample = 8 << 10
	t0 := time.Now()
	status, body, err := ProbeRangeGETHeaders(ctx, client, rawURL, 0, sample-1, extra)
	ms := time.Since(t0).Milliseconds()
	if err != nil || (status >= 400 && status != 416) || len(body) == 0 {
		status2, body2, err2 := ProbeGETHeaders(ctx, client, rawURL, sample, extra)
		ms = time.Since(t0).Milliseconds()
		if err2 != nil {
			detail := fmt.Sprintf("%s: Range %v", rawURL, err)
			if err == nil {
				detail = fmt.Sprintf("%s → Range HTTP %d", rawURL, status)
			}
			detail += fmt.Sprintf("; GET %v", err2)
			return AccessProbeCheck{Name: name, OK: false, MS: ms, Detail: detail}
		}
		if status2 >= 400 || len(body2) == 0 {
			return AccessProbeCheck{
				Name: name, OK: false, MS: ms,
				Detail: fmt.Sprintf("%s → HTTP %d，读取 %d 字节（期望下载到内容）", rawURL, status2, len(body2)),
			}
		}
		return AccessProbeCheck{
			Name: name, OK: true, MS: ms,
			Detail: fmt.Sprintf("%s → HTTP %d，已下载 %d 字节", rawURL, status2, len(body2)),
		}
	}
	return AccessProbeCheck{
		Name: name, OK: true, MS: ms,
		Detail: fmt.Sprintf("%s → HTTP %d，已下载 %d 字节（Range）", rawURL, status, len(body)),
	}
}

// ProbeURLReachable 对 URL 做 GET，仅 HTTP 2xx/3xx 视为可达（4xx 不算通过）。
func ProbeURLReachable(ctx context.Context, client *http.Client, name, rawURL string) AccessProbeCheck {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || rawURL == "/" {
		return AccessProbeCheck{Name: name, OK: false, Detail: "上游地址为空"}
	}
	t0 := time.Now()
	status, _, err := ProbeGET(ctx, client, rawURL, 64<<10)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		return AccessProbeCheck{
			Name: name, OK: false, MS: ms,
			Detail: fmt.Sprintf("%s: %v", rawURL, err),
		}
	}
	ok := status >= 200 && status < 400
	return AccessProbeCheck{
		Name: name, OK: ok, MS: ms,
		Detail: fmt.Sprintf("%s → HTTP %d", rawURL, status),
	}
}

// JoinURL 拼接 base 与 path。
func JoinURL(base, suffix string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		return ""
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return b + suffix
}

func copyExtraHeaders(dst http.Header, extra http.Header) {
	if extra == nil {
		return
	}
	for k, vs := range extra {
		for _, v := range vs {
			dst.Set(k, v)
		}
	}
}
