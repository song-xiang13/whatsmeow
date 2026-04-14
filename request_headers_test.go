package whatsmeow

import (
	"net/http"
	"testing"

	"github.com/song-xiang13/whatsmeow/store"
)

func TestSanitizeAllowedRequestHeaders(t *testing.T) {
	input := http.Header{
		"accept-language":    {"zh-CN,zh;q=0.9"},
		"Cache-Control":      {"no-cache"},
		"Pragma":             {"no-cache"},
		"User-Agent":         {"Mozilla/5.0"},
		"Cookie":             {"wa_web_lang_pref=zh_CN"},
		"Sec-WebSocket-Key":  {"abc"},
		"Accept-Encoding":    {"gzip, deflate, br"},
		"X-Custom-Untrusted": {"ignored"},
	}

	got := sanitizeAllowedRequestHeaders(input)

	if got.Get("Accept-Language") != "zh-CN,zh;q=0.9" {
		t.Fatalf("unexpected Accept-Language: got %q", got.Get("Accept-Language"))
	}
	if got.Get("Cache-Control") != "no-cache" {
		t.Fatalf("unexpected Cache-Control: got %q", got.Get("Cache-Control"))
	}
	if got.Get("Pragma") != "no-cache" {
		t.Fatalf("unexpected Pragma: got %q", got.Get("Pragma"))
	}
	if got.Get("User-Agent") != "Mozilla/5.0" {
		t.Fatalf("unexpected User-Agent: got %q", got.Get("User-Agent"))
	}
	if got.Get("Cookie") != "" {
		t.Fatalf("expected Cookie to be stripped, got %q", got.Get("Cookie"))
	}
	if got.Get("Sec-WebSocket-Key") != "" {
		t.Fatalf("expected Sec-WebSocket-Key to be stripped, got %q", got.Get("Sec-WebSocket-Key"))
	}
	if got.Get("Accept-Encoding") != "" {
		t.Fatalf("expected Accept-Encoding to be stripped, got %q", got.Get("Accept-Encoding"))
	}
	if got.Get("X-Custom-Untrusted") != "" {
		t.Fatalf("expected custom untrusted header to be stripped, got %q", got.Get("X-Custom-Untrusted"))
	}
}

func TestWithAllowedRequestHeaders(t *testing.T) {
	cli := NewClient(&store.Device{}, nil, WithAllowedRequestHeaders(AllowedRequestHeaders{
		Websocket: http.Header{
			"Accept-Language": {"zh-CN,zh;q=0.9"},
			"User-Agent":      {"Mozilla/5.0"},
		},
		Media: http.Header{
			"Cache-Control": {"no-cache"},
			"Cookie":        {"wa_web_lang_pref=zh_CN"},
		},
	}))

	if got := cli.websocketHeaderOverrides.Get("Accept-Language"); got != "zh-CN,zh;q=0.9" {
		t.Fatalf("unexpected websocket Accept-Language: got %q", got)
	}
	if got := cli.websocketHeaderOverrides.Get("User-Agent"); got != "Mozilla/5.0" {
		t.Fatalf("unexpected websocket User-Agent: got %q", got)
	}
	if got := cli.mediaHeaderOverrides.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unexpected media Cache-Control: got %q", got)
	}
	if got := cli.mediaHeaderOverrides.Get("Cookie"); got != "" {
		t.Fatalf("expected media Cookie to be stripped, got %q", got)
	}
}

func TestApplyHeaderOverrides(t *testing.T) {
	dst := http.Header{
		"Origin":          {"https://web.whatsapp.com"},
		"Accept-Language": {"en-US"},
		"Cache-Control":   {"max-age=0"},
	}
	overrides := http.Header{
		"Accept-Language": {"zh-CN,zh;q=0.9"},
		"Cache-Control":   {"no-cache"},
	}

	applyHeaderOverrides(dst, overrides)

	if got := dst.Get("Origin"); got != "https://web.whatsapp.com" {
		t.Fatalf("unexpected Origin: got %q", got)
	}
	if got := dst.Values("Accept-Language"); len(got) != 1 || got[0] != "zh-CN,zh;q=0.9" {
		t.Fatalf("unexpected Accept-Language values: got %v", got)
	}
	if got := dst.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unexpected Cache-Control: got %q", got)
	}
}
