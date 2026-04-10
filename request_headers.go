package whatsmeow

import "net/http"

// AllowedRequestHeaders contains opt-in request header overrides for test environments.
//
// Only low-risk headers are supported here. Sensitive transport and identity headers such as
// Host, Connection, Upgrade, Sec-WebSocket-*, User-Agent, Cookie and Accept-Encoding are
// intentionally ignored.
type AllowedRequestHeaders struct {
	Websocket http.Header
	Media     http.Header
}

var allowedRequestHeaderNames = map[string]struct{}{
	"Accept-Language": {},
	"Cache-Control":   {},
	"Pragma":          {},
}

func sanitizeAllowedRequestHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}

	sanitized := make(http.Header)
	for key, values := range headers {
		canonicalKey := http.CanonicalHeaderKey(key)
		if _, ok := allowedRequestHeaderNames[canonicalKey]; !ok {
			continue
		}
		sanitized[canonicalKey] = append([]string(nil), values...)
	}
	if len(sanitized) == 0 {
		return nil
	}
	return sanitized
}

func cloneHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func applyHeaderOverrides(dst, overrides http.Header) {
	if len(dst) == 0 || len(overrides) == 0 {
		return
	}
	for key, values := range overrides {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
