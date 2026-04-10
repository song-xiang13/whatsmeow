package whatsmeow

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const defaultAcceptEncoding = "gzip, deflate, br, zstd"

type contentEncodingTransport struct {
	base http.RoundTripper
}

func newContentEncodingTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &contentEncodingTransport{base: base}
}

func (t *contentEncodingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clonedReq := req.Clone(req.Context())
	clonedReq.Header = req.Header.Clone()
	if clonedReq.Header.Get("Accept-Encoding") == "" {
		clonedReq.Header.Set("Accept-Encoding", defaultAcceptEncoding)
	}

	resp, err := t.base.RoundTrip(clonedReq)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		return resp, nil
	}

	decodedBody, decoded, err := decodeResponseBody(resp.Header.Get("Content-Encoding"), resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if !decoded {
		return resp, nil
	}

	resp.Body = decodedBody
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

func decodeResponseBody(contentEncoding string, body io.ReadCloser) (io.ReadCloser, bool, error) {
	encoding := strings.TrimSpace(strings.ToLower(contentEncoding))
	if len(encoding) == 0 || encoding == "identity" {
		return body, false, nil
	}
	if idx := strings.IndexByte(encoding, ';'); idx >= 0 {
		encoding = strings.TrimSpace(encoding[:idx])
	}

	switch encoding {
	case "gzip":
		reader, err := gzip.NewReader(body)
		if err != nil {
			return nil, false, fmt.Errorf("decode gzip response: %w", err)
		}
		return &stackedReadCloser{Reader: reader, closers: []io.Closer{reader, body}}, true, nil
	case "deflate":
		reader, err := zlib.NewReader(body)
		if err != nil {
			return nil, false, fmt.Errorf("decode deflate response: %w", err)
		}
		return &stackedReadCloser{Reader: reader, closers: []io.Closer{reader, body}}, true, nil
	case "br":
		return &stackedReadCloser{Reader: brotli.NewReader(body), closers: []io.Closer{body}}, true, nil
	case "zstd":
		reader, err := zstd.NewReader(body)
		if err != nil {
			return nil, false, fmt.Errorf("decode zstd response: %w", err)
		}
		return &zstdReadCloser{Decoder: reader, body: body}, true, nil
	default:
		return body, false, nil
	}
}

type stackedReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (src *stackedReadCloser) Close() error {
	var firstErr error
	for _, closer := range src.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type zstdReadCloser struct {
	*zstd.Decoder
	body io.Closer
}

func (zrc *zstdReadCloser) Close() error {
	zrc.Decoder.Close()
	if zrc.body != nil {
		return zrc.body.Close()
	}
	return nil
}
