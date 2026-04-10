package whatsmeow

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestContentEncodingTransportSetsAcceptEncoding(t *testing.T) {
	rt := &roundTripFunc{fn: func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Accept-Encoding"); got != defaultAcceptEncoding {
			t.Fatalf("unexpected Accept-Encoding: got %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    req,
		}, nil
	}}

	client := newContentEncodingTransport(rt)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	defer resp.Body.Close()
}

func TestContentEncodingTransportPreservesExistingAcceptEncoding(t *testing.T) {
	rt := &roundTripFunc{fn: func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Fatalf("unexpected Accept-Encoding: got %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    req,
		}, nil
	}}

	client := newContentEncodingTransport(rt)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	defer resp.Body.Close()
}

func TestContentEncodingTransportDecodesResponses(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "gzip", encoding: "gzip", body: mustEncodeGzip(t, "hello")},
		{name: "deflate", encoding: "deflate", body: mustEncodeDeflate(t, "hello")},
		{name: "br", encoding: "br", body: mustEncodeBrotli(t, "hello")},
		{name: "zstd", encoding: "zstd", body: mustEncodeZstd(t, "hello")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &roundTripFunc{fn: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Encoding": {tc.encoding},
						"Content-Length":   {strconv.Itoa(len(tc.body))},
					},
					Body:    io.NopCloser(bytes.NewReader(tc.body)),
					Request: req,
				}, nil
			}}

			client := newContentEncodingTransport(rt)
			req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if err != nil {
				t.Fatalf("failed to build request: %v", err)
			}

			resp, err := client.RoundTrip(req)
			if err != nil {
				t.Fatalf("round trip failed: %v", err)
			}
			defer resp.Body.Close()

			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body failed: %v", err)
			}
			if got := string(data); got != "hello" {
				t.Fatalf("unexpected decoded body: got %q", got)
			}
			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("expected Content-Encoding to be cleared, got %q", got)
			}
			if resp.ContentLength != -1 {
				t.Fatalf("expected ContentLength to be -1, got %d", resp.ContentLength)
			}
		})
	}
}

type roundTripFunc struct {
	fn func(*http.Request) (*http.Response, error)
}

func (rt *roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt.fn(req)
}

func mustEncodeGzip(t *testing.T, input string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	return buf.Bytes()
}

func mustEncodeDeflate(t *testing.T, input string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatalf("deflate write failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("deflate close failed: %v", err)
	}
	return buf.Bytes()
}

func mustEncodeBrotli(t *testing.T, input string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatalf("brotli write failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("brotli close failed: %v", err)
	}
	return buf.Bytes()
}

func mustEncodeZstd(t *testing.T, input string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer init failed: %v", err)
	}
	if _, err = writer.Write([]byte(input)); err != nil {
		t.Fatalf("zstd write failed: %v", err)
	}
	writer.Close()
	return buf.Bytes()
}
