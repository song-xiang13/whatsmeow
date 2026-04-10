package whatsmeow

import (
	"context"
	gotls "crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

type utlsTransportProfile struct {
	nextProtos        []string
	forceAttemptHTTP2 bool
}

var (
	mediaUTLSProfile = utlsTransportProfile{
		nextProtos:        []string{"h2", "http/1.1"},
		forceAttemptHTTP2: true,
	}
	mediaHTTP1OnlyUTLSProfile = utlsTransportProfile{
		nextProtos: []string{"http/1.1"},
	}
	mediaHTTP2OnlyUTLSProfile = utlsTransportProfile{
		nextProtos: []string{"h2"},
	}
	websocketUTLSProfile = utlsTransportProfile{
		nextProtos: []string{"http/1.1"},
	}
)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// ParseUTLSClientHelloHex converts a hex-encoded TLS ClientHello record into raw bytes.
//
// The input must include the full TLS record, not only the inner handshake payload.
func ParseUTLSClientHelloHex(value string) ([]byte, error) {
	normalized := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case r >= 'a' && r <= 'f':
			return r
		case r >= 'A' && r <= 'F':
			return r
		default:
			return -1
		}
	}, value)
	if normalized == "" {
		return nil, fmt.Errorf("empty ClientHello hex")
	}

	raw, err := hex.DecodeString(normalized)
	if err != nil {
		return nil, fmt.Errorf("decode ClientHello hex: %w", err)
	}
	return raw, validateUTLSClientHelloRecord(raw)
}

// WithUTLSClientHelloRecord configures the managed HTTP and websocket clients to reuse a captured ClientHello record.
//
// The record should be a full TLS record, as captured from the wire.
func WithUTLSClientHelloRecord(raw []byte) ClientOption {
	cloned := append([]byte(nil), raw...)
	return func(cli *Client) {
		cli.utlsClientHelloRecord = cloned
	}
}

func newDefaultMediaHTTPClient(clientHelloRecord []byte) *http.Client {
	if len(clientHelloRecord) == 0 {
		return &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		}
	}
	return &http.Client{
		Transport: wrapManagedMediaTransport(nil, clientHelloRecord),
	}
}

func newDefaultWebsocketHTTPClient(clientHelloRecord []byte) *http.Client {
	if len(clientHelloRecord) == 0 {
		return &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		}
	}
	return &http.Client{
		Transport: wrapManagedWebsocketTransport(nil, clientHelloRecord),
	}
}

func wrapManagedMediaTransport(base *http.Transport, clientHelloRecord []byte) http.RoundTripper {
	if len(clientHelloRecord) == 0 {
		if base == nil {
			return http.DefaultTransport.(*http.Transport).Clone()
		}
		return base.Clone()
	}
	return newContentEncodingTransport(newFallbackRoundTripper(
		newUTLSHTTP2Transport(base, clientHelloRecord),
		newUTLSTransport(base, mediaHTTP1OnlyUTLSProfile, clientHelloRecord),
	))
}

func wrapManagedWebsocketTransport(base *http.Transport, clientHelloRecord []byte) http.RoundTripper {
	if len(clientHelloRecord) == 0 {
		if base == nil {
			return http.DefaultTransport.(*http.Transport).Clone()
		}
		return base.Clone()
	}
	return newContentEncodingTransport(newUTLSTransport(base, websocketUTLSProfile, clientHelloRecord))
}

type fallbackRoundTripper struct {
	primary  http.RoundTripper
	fallback http.RoundTripper
}

func newFallbackRoundTripper(primary, fallback http.RoundTripper) http.RoundTripper {
	return &fallbackRoundTripper{
		primary:  primary,
		fallback: fallback,
	}
}

func (frt *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if frt == nil || frt.primary == nil {
		return nil, fmt.Errorf("primary round tripper is nil")
	}

	primaryReq, fallbackReq, err := cloneRequestForProtocolFallback(req)
	if err != nil {
		return nil, err
	}

	resp, err := frt.primary.RoundTrip(primaryReq)
	if err == nil || frt.fallback == nil || fallbackReq == nil || !shouldFallbackToHTTP1(err) {
		return resp, err
	}

	return frt.fallback.RoundTrip(fallbackReq)
}

func cloneRequestForProtocolFallback(req *http.Request) (*http.Request, *http.Request, error) {
	primaryReq := req.Clone(req.Context())
	primaryReq.Header = req.Header.Clone()
	primaryReq.Body = req.Body

	if req.Body == nil {
		fallbackReq := req.Clone(req.Context())
		fallbackReq.Header = req.Header.Clone()
		return primaryReq, fallbackReq, nil
	}
	if req.GetBody == nil {
		return primaryReq, nil, nil
	}

	fallbackBody, err := req.GetBody()
	if err != nil {
		return nil, nil, err
	}
	fallbackReq := req.Clone(req.Context())
	fallbackReq.Header = req.Header.Clone()
	fallbackReq.Body = fallbackBody
	fallbackReq.GetBody = req.GetBody
	fallbackReq.ContentLength = req.ContentLength
	return primaryReq, fallbackReq, nil
}

func shouldFallbackToHTTP1(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "malformed HTTP status code") ||
		strings.Contains(msg, "HTTP/1.x transport connection broken") ||
		strings.Contains(msg, "unexpected ALPN protocol")
}

func newUTLSTransport(base *http.Transport, profile utlsTransportProfile, clientHelloRecord []byte) *http.Transport {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		base = base.Clone()
	}

	dialContext := base.DialContext
	if dialContext == nil {
		defaultDialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dialContext = defaultDialer.DialContext
	}

	baseTLSConfig := cloneGoTLSConfig(base.TLSClientConfig)
	base.TLSClientConfig = setGoTLSNextProtos(baseTLSConfig, profile.nextProtos)
	clonedRecord := append([]byte(nil), clientHelloRecord...)
	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialUTLSContext(ctx, dialContext, baseTLSConfig, network, addr, profile, clonedRecord)
	}
	base.ForceAttemptHTTP2 = profile.forceAttemptHTTP2

	return base
}

func newUTLSHTTP2Transport(base *http.Transport, clientHelloRecord []byte) *http2.Transport {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		base = base.Clone()
	}

	dialContext := base.DialContext
	if dialContext == nil {
		defaultDialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dialContext = defaultDialer.DialContext
	}

	baseTLSConfig := cloneGoTLSConfig(base.TLSClientConfig)
	clonedRecord := append([]byte(nil), clientHelloRecord...)
	return &http2.Transport{
		DisableCompression: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *gotls.Config) (net.Conn, error) {
			return dialUTLSContext(ctx, dialContext, baseTLSConfig, network, addr, mediaHTTP2OnlyUTLSProfile, clonedRecord)
		},
	}
}

func dialUTLSContext(
	ctx context.Context,
	dialContext dialContextFunc,
	baseTLSConfig *gotls.Config,
	network, addr string,
	profile utlsTransportProfile,
	clientHelloRecord []byte,
) (net.Conn, error) {
	rawConn, err := dialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	serverName := hostnameFromAddr(addr)
	uTLSConfig := newUTLSConfig(baseTLSConfig, serverName, profile.nextProtos)
	uconn, err := newUTLSConn(rawConn, uTLSConfig, profile, clientHelloRecord)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("build uTLS ClientHello for %s: %w", addr, err)
	}
	if err = uconn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("uTLS handshake failed for %s: %w", addr, err)
	}

	return uconn, nil
}

func newUTLSConn(rawConn net.Conn, cfg *utls.Config, profile utlsTransportProfile, clientHelloRecord []byte) (*utls.UConn, error) {
	spec, err := buildUTLSClientHelloSpec(clientHelloRecord, profile)
	if err != nil {
		return nil, err
	}

	uconn := utls.UClient(rawConn, cfg, utls.HelloCustom)
	if err = uconn.ApplyPreset(spec); err != nil {
		return nil, err
	}
	uconn.SetSNI(cfg.ServerName)
	return uconn, nil
}

func buildUTLSClientHelloSpec(clientHelloRecord []byte, profile utlsTransportProfile) (*utls.ClientHelloSpec, error) {
	var (
		spec utls.ClientHelloSpec
		err  error
	)

	if len(clientHelloRecord) > 0 {
		fingerprinter := &utls.Fingerprinter{}
		var parsed *utls.ClientHelloSpec
		parsed, err = fingerprinter.FingerprintClientHello(clientHelloRecord)
		if err != nil {
			return nil, fmt.Errorf("fingerprint ClientHello record: %w", err)
		}
		spec = *parsed
	} else {
		spec, err = utls.UTLSIdToSpec(utls.HelloChrome_Auto)
		if err != nil {
			return nil, fmt.Errorf("load default Chrome ClientHello: %w", err)
		}
	}

	overrideALPNInSpec(&spec, profile.nextProtos)
	return &spec, nil
}

func overrideALPNInSpec(spec *utls.ClientHelloSpec, nextProtos []string) {
	if spec == nil {
		return
	}

	alpnProtocols := cloneStrings(nextProtos)
	for _, ext := range spec.Extensions {
		alpnExt, ok := ext.(*utls.ALPNExtension)
		if ok {
			alpnExt.AlpnProtocols = alpnProtocols
			return
		}
	}
	if len(alpnProtocols) > 0 {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: alpnProtocols})
	}
}

func validateUTLSClientHelloRecord(raw []byte) error {
	if len(raw) < 9 {
		return fmt.Errorf("ClientHello record too short: got %d bytes", len(raw))
	}
	if raw[0] != 0x16 {
		return fmt.Errorf("unexpected TLS record type 0x%02x (want handshake 0x16)", raw[0])
	}
	recordLen := int(raw[3])<<8 | int(raw[4])
	if recordLen != len(raw)-5 {
		return fmt.Errorf("TLS record length mismatch: header says %d, got %d payload bytes", recordLen, len(raw)-5)
	}
	if raw[5] != 0x01 {
		return fmt.Errorf("unexpected handshake type 0x%02x (want ClientHello 0x01)", raw[5])
	}
	helloLen := int(raw[6])<<16 | int(raw[7])<<8 | int(raw[8])
	if helloLen != len(raw)-9 {
		return fmt.Errorf("ClientHello length mismatch: header says %d, got %d payload bytes", helloLen, len(raw)-9)
	}
	return nil
}

func cloneGoTLSConfig(cfg *gotls.Config) *gotls.Config {
	if cfg == nil {
		return nil
	}
	return cfg.Clone()
}

func setGoTLSNextProtos(cfg *gotls.Config, nextProtos []string) *gotls.Config {
	if cfg == nil {
		cfg = &gotls.Config{}
	} else {
		cfg = cfg.Clone()
	}
	cfg.NextProtos = cloneStrings(nextProtos)
	return cfg
}

func newUTLSConfig(base *gotls.Config, serverName string, nextProtos []string) *utls.Config {
	cfg := &utls.Config{
		ServerName:         serverName,
		NextProtos:         cloneStrings(nextProtos),
		ClientSessionCache: utls.NewLRUClientSessionCache(32),
	}
	if base == nil {
		return cfg
	}

	if base.ServerName != "" {
		cfg.ServerName = base.ServerName
	}

	cfg.InsecureSkipVerify = base.InsecureSkipVerify
	cfg.RootCAs = base.RootCAs
	cfg.MinVersion = base.MinVersion
	cfg.MaxVersion = base.MaxVersion
	cfg.KeyLogWriter = base.KeyLogWriter
	cfg.SessionTicketsDisabled = base.SessionTicketsDisabled
	if cfg.SessionTicketsDisabled {
		cfg.ClientSessionCache = nil
	}

	return cfg
}

func hostnameFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host
	}
	return addr
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}
