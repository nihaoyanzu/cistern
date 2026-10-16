package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Head 探测上游文件大小（用于 boost / 分片 / Docker descriptor 校验）。
// 成功时保证 size>0；拿不到有效大小则返回 error（绝不把 -1/0 当成功）。
func (e *Engine) Head(ctx context.Context, rawURL string, headers http.Header) (int64, string, string, error) {
	return e.head(ctx, Options{URL: rawURL, Headers: headers})
}

func (e *Engine) head(ctx context.Context, opt Options) (int64, string, string, error) {
	size, ct, finalURL, _, err := e.headWithETag(ctx, opt)
	return size, ct, finalURL, err
}

func (e *Engine) headWithETag(ctx context.Context, opt Options) (int64, string, string, string, error) {
	ctx, _ = WithHubFileMeta(ctx)
	// 使用独立的 headClient，不复用连接池，避免卡死连接阻塞
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, opt.URL, nil)
	if err != nil {
		return 0, "", "", "", err
	}
	copyHeaders(req, opt.Headers)
	req.Header.Del("Accept-Encoding")
	resp, err := e.headClient.Do(req)
	if err != nil {
		return 0, "", "", "", wrapClassified(err, false)
	}
	finalURL := resp.Request.URL.String()
	ct := resp.Header.Get("Content-Type")
	etag := resp.Header.Get("ETag")
	status := resp.StatusCode
	cl := resp.ContentLength
	_ = resp.Body.Close()
	if status >= 400 {
		return 0, "", "", "", statusError(status, resp.Header, opt.OnAuthFail != nil)
	}
	if cl > 0 {
		return cl, ct, finalURL, etag, nil
	}
	// 无 Content-Length：用 Range bytes=0-0 从 Content-Range 解析 total（containerd 依赖真实 size）
	size, rangeCT, rangeURL, rangeETag, rangeErr := e.headSizeViaRangeETag(ctx, opt)
	if rangeErr != nil {
		if cl == 0 {
			return 0, "", "", "", fmt.Errorf("HEAD %s: content size of zero", opt.URL)
		}
		return 0, "", "", "", fmt.Errorf("HEAD %s: missing content length: %w", opt.URL, rangeErr)
	}
	if rangeCT != "" {
		ct = rangeCT
	}
	if rangeURL != "" {
		finalURL = rangeURL
	}
	if rangeETag != "" {
		etag = rangeETag
	}
	return size, ct, finalURL, etag, nil
}

// headSizeViaRange 发 Range: bytes=0-0，从 Content-Range 解析总长度。
func (e *Engine) headSizeViaRange(ctx context.Context, opt Options) (int64, string, string, error) {
	size, ct, u, _, err := e.headSizeViaRangeETag(ctx, opt)
	return size, ct, u, err
}

func (e *Engine) headSizeViaRangeETag(ctx context.Context, opt Options) (int64, string, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opt.URL, nil)
	if err != nil {
		return 0, "", "", "", err
	}
	copyHeaders(req, opt.Headers)
	req.Header.Del("Accept-Encoding")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := e.headClient.Do(req)
	if err != nil {
		return 0, "", "", "", wrapClassified(err, false)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 {
			return 0, "", "", "", statusError(resp.StatusCode, resp.Header, opt.OnAuthFail != nil)
		}
		return 0, "", "", "", fmt.Errorf("Range probe unexpected status %s", resp.Status)
	}
	size := parseContentRangeTotal(resp.Header.Get("Content-Range"))
	if size <= 0 && resp.ContentLength > 0 && resp.StatusCode == http.StatusOK {
		// 上游忽略 Range 回了整包：CL 即总长
		size = resp.ContentLength
	}
	if size <= 0 {
		return 0, "", "", "", fmt.Errorf("Range probe: cannot resolve size")
	}
	return size, resp.Header.Get("Content-Type"), resp.Request.URL.String(), resp.Header.Get("ETag"), nil
}

// parseContentRangeTotal 解析 Content-Range: bytes start-end/total；* 或无效返回 -1。
func parseContentRangeTotal(cr string) int64 {
	cr = strings.TrimSpace(cr)
	if cr == "" {
		return -1
	}
	i := strings.LastIndexByte(cr, '/')
	if i < 0 || i+1 >= len(cr) {
		return -1
	}
	total := strings.TrimSpace(cr[i+1:])
	if total == "*" || total == "" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}
