package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 恢复/放弃用的错误类别（短标签，不是文案清单）。
type Class int

const (
	ClassOK             Class = iota
	ClassTransient            // A：断连/stall/超时/5xx≠503 → 续传
	ClassRateLimit            // B：429/503 → 降压再续
	ClassAuthRetry            // E：401/403 且还能换票一次
	ClassAuthPermanent        // E′：鉴权不可恢复
	ClassContentChanged       // F：内容变更 → 作废重来
	ClassHard                 // C：404 等硬失败
	ClassCancel               // D：本地取消
)

// 结构化原因码（队列/前端展示）。
const (
	CodeTransientExhausted = "transient_exhausted"
	CodeRateLimited        = "rate_limited"
	CodeStalled            = "stalled"
	CodeAuthRequired       = "auth_required"
	CodeAuthFailed         = "auth_failed"
	CodeForbidden          = "forbidden"
	CodeContentChanged     = "content_changed"
	CodeDigestMismatch     = "digest_mismatch"
	CodeNotFound           = "not_found"
	CodeBadContent         = "upstream_bad_content"
	CodeCancelled          = "cancelled"
)

var (
	errUpstreamRateLimited = errors.New("upstream rate limited")
	errUpstreamAuth        = errors.New("upstream auth failed")
	errContentChanged      = errors.New("upstream content changed")
	errDigestMismatch      = errors.New("digest mismatch")
	errUpstreamStall       = errors.New("upstream read stalled")
	// pause 期间停读后连接可能已僵死，调用方应换新请求重试
	errPrefetchReconnect = errors.New("prefetch reconnect after pause")
)

// FailError 带稳定 code 的失败，供调度器 End 与前端展示。
type FailError struct {
	Code   string
	Msg    string
	Cause  error
	Class  Class
	Retry  time.Duration // Retry-After，可选
	Status int
}

func (e *FailError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" && e.Msg != "" {
		return e.Code + ": " + e.Msg
	}
	if e.Code != "" {
		return e.Code
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "download failed"
}

func (e *FailError) Unwrap() error { return e.Cause }

func fail(code, msg string, class Class, cause error) *FailError {
	return &FailError{Code: code, Msg: msg, Class: class, Cause: cause}
}

// ErrorCode 提取失败码；无则空。
func ErrorCode(err error) string {
	var fe *FailError
	if errors.As(err, &fe) && fe != nil {
		return fe.Code
	}
	return ""
}

// ClassOf 提取类别。
func ClassOf(err error) Class {
	if err == nil {
		return ClassOK
	}
	var fe *FailError
	if errors.As(err, &fe) && fe != nil && fe.Class != ClassOK {
		return fe.Class
	}
	return classifyUpstream(err, 0).Class
}

type classified struct {
	Class      Class
	Code       string
	RetryAfter time.Duration
	Status     int
}

// classifyUpstream 单一入口：HTTP 状态与净错误 → 类别。
// authCanRetry：调用方尚未换票且提供了 OnAuthFail 时为 true。
func classifyUpstream(err error, httpStatus int) classified {
	return classifyUpstreamAuth(err, httpStatus, false)
}

func classifyUpstreamAuth(err error, httpStatus int, authCanRetry bool) classified {
	if err == nil && httpStatus == 0 {
		return classified{Class: ClassOK}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return classified{Class: ClassCancel, Code: CodeCancelled}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// 本地 deadline 与上游超时难以区分；有进度续传场景按瞬时处理由调用方决定
			if isTransientNetErr(err) {
				return classified{Class: ClassTransient, Code: CodeTransientExhausted}
			}
			return classified{Class: ClassCancel, Code: CodeCancelled}
		}
		if errors.Is(err, errUpstreamStall) {
			return classified{Class: ClassTransient, Code: CodeStalled}
		}
		if errors.Is(err, errPrefetchReconnect) {
			return classified{Class: ClassTransient}
		}
		if errors.Is(err, errUpstreamRateLimited) {
			return classified{Class: ClassRateLimit, Code: CodeRateLimited}
		}
		if errors.Is(err, errContentChanged) {
			return classified{Class: ClassContentChanged, Code: CodeContentChanged}
		}
		low := strings.ToLower(err.Error())
		if errors.Is(err, errDigestMismatch) ||
			(strings.Contains(low, "mismatch") &&
				(strings.Contains(low, "sha256") || strings.Contains(low, "sha512") || strings.Contains(low, "digest"))) {
			return classified{Class: ClassContentChanged, Code: CodeDigestMismatch}
		}
		if errors.Is(err, errUpstreamAuth) {
			if authCanRetry {
				return classified{Class: ClassAuthRetry, Code: CodeAuthFailed}
			}
			return classified{Class: ClassAuthPermanent, Code: CodeAuthFailed}
		}
		if isTransientNetErr(err) {
			return classified{Class: ClassTransient, Code: CodeTransientExhausted}
		}
		// 从错误串解析 chunk status / upstream 401
		if st := parseStatusFromErr(err); st > 0 {
			httpStatus = st
		}
	}

	if httpStatus > 0 {
		return classifyHTTP(httpStatus, authCanRetry, nil)
	}
	return classified{Class: ClassHard, Code: CodeBadContent}
}

func classifyHTTP(status int, authCanRetry bool, hdr http.Header) classified {
	c := classified{Status: status}
	switch {
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		c.Class = ClassRateLimit
		c.Code = CodeRateLimited
		if hdr != nil {
			c.RetryAfter = parseRetryAfter(hdr.Get("Retry-After"))
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if authCanRetry {
			c.Class = ClassAuthRetry
			c.Code = CodeAuthFailed
		} else if status == http.StatusUnauthorized {
			c.Class = ClassAuthPermanent
			c.Code = CodeAuthRequired
		} else {
			c.Class = ClassAuthPermanent
			c.Code = CodeForbidden
		}
	case status == http.StatusNotFound || status == http.StatusGone:
		c.Class = ClassHard
		c.Code = CodeNotFound
	case status >= 500:
		c.Class = ClassTransient
		c.Code = CodeTransientExhausted
	case status >= 400:
		c.Class = ClassHard
		c.Code = CodeBadContent
	default:
		c.Class = ClassOK
	}
	return c
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if sec, err := time.ParseDuration(v + "s"); err == nil && sec > 0 {
		return sec
	}
	// HTTP-date 忽略，用短默认
	return 0
}

func parseStatusFromErr(err error) int {
	if err == nil {
		return 0
	}
	s := err.Error()
	// "chunk status 429" / "upstream 401 Unauthorized" / "Range probe ...: 403 Forbidden"
	for _, p := range []string{"chunk status ", "upstream ", "status "} {
		if i := strings.Index(strings.ToLower(s), p); i >= 0 {
			rest := s[i+len(p):]
			var n int
			if _, err := fmt.Sscanf(rest, "%d", &n); err == nil && n >= 100 && n < 600 {
				return n
			}
		}
	}
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "401"), strings.Contains(low, "unauthorized"):
		return 401
	case strings.Contains(low, "403"), strings.Contains(low, "forbidden"):
		return 403
	case strings.Contains(low, "429"):
		return 429
	case strings.Contains(low, "503"):
		return 503
	case strings.Contains(low, "404"):
		return 404
	}
	return 0
}

// isRecoverable 是否走续传/降压预算（不含鉴权换票与内容重来，那些单独预算）。
func isRecoverable(c Class) bool {
	return c == ClassTransient || c == ClassRateLimit || c == ClassAuthRetry
}

// shouldAbandonImmediately 不可恢复 → 立刻放弃。
func shouldAbandonImmediately(c Class) bool {
	return c == ClassHard || c == ClassAuthPermanent || c == ClassCancel
}

func statusError(status int, hdr http.Header, authCanRetry bool) error {
	c := classifyHTTP(status, authCanRetry, hdr)
	msg := fmt.Sprintf("upstream status %d", status)
	switch c.Class {
	case ClassRateLimit:
		return &FailError{Code: c.Code, Msg: msg, Class: c.Class, Cause: errUpstreamRateLimited, Retry: c.RetryAfter, Status: status}
	case ClassAuthRetry, ClassAuthPermanent:
		return &FailError{Code: c.Code, Msg: msg, Class: c.Class, Cause: errUpstreamAuth, Status: status}
	case ClassHard:
		return &FailError{Code: c.Code, Msg: msg, Class: c.Class, Status: status}
	case ClassTransient:
		return &FailError{Code: c.Code, Msg: msg, Class: c.Class, Status: status}
	default:
		return fmt.Errorf("upstream status %d", status)
	}
}

func wrapClassified(err error, authCanRetry bool) error {
	if err == nil {
		return nil
	}
	var fe *FailError
	if errors.As(err, &fe) {
		return err
	}
	c := classifyUpstreamAuth(err, 0, authCanRetry)
	if c.Class == ClassOK {
		return err
	}
	return &FailError{Code: c.Code, Msg: err.Error(), Class: c.Class, Cause: err, Status: c.Status, Retry: c.RetryAfter}
}
