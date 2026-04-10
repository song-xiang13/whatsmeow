package whatsmeow

import (
	gotls "crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	utls "github.com/refraction-networking/utls"
)

const sampleClientHelloHex = "1603010200010001fc030392740c68aba1ee226cfb15d9dbf3bedbc7f9beea0a2dbd10a73163ed8bb1787820d39d5f23f8949806e1e759911b56bd1d15b07eaf360623bae891c855b8e233120036aaaa130113021303c02cc02bcca9c030c02fcca8c024c023c00ac009c028c027c014c013009d009c003d003c0035002fc008c012000a0100017d0a0a00000000001f001d00001a756a70352e786e2d2d6768717535666d3237623637772e636f6d00170000ff01000100000a000c000a2a2a001d001700180019000b000201000010000b000908687474702f312e31000500050100000000000d0018001604030804040105030203080508050501080606010201001200000033002b00292a2a000100001d0020032859de395bae1e2d2d61fb5cbe98aa2828e5b9fbaca6375f9f72603f46da57002d00020101002b000b0adada0304030303020301fafa000100001500b200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

func TestParseUTLSClientHelloHex(t *testing.T) {
	raw, err := ParseUTLSClientHelloHex(sampleClientHelloHex)
	if err != nil {
		t.Fatalf("failed to parse sample ClientHello hex: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("parsed ClientHello record should not be empty")
	}
}

func TestBuildUTLSClientHelloSpecOverridesALPN(t *testing.T) {
	spec, err := buildUTLSClientHelloSpec(nil, websocketUTLSProfile)
	if err != nil {
		t.Fatalf("failed to build default ClientHello spec: %v", err)
	}
	if got := getALPNProtocols(spec); !reflect.DeepEqual(got, websocketUTLSProfile.nextProtos) {
		t.Fatalf("unexpected websocket ALPN list: %v", got)
	}

	raw, err := ParseUTLSClientHelloHex(sampleClientHelloHex)
	if err != nil {
		t.Fatalf("failed to parse sample ClientHello hex: %v", err)
	}
	spec, err = buildUTLSClientHelloSpec(raw, mediaUTLSProfile)
	if err != nil {
		t.Fatalf("failed to build fingerprinted ClientHello spec: %v", err)
	}
	if got := getALPNProtocols(spec); !reflect.DeepEqual(got, mediaUTLSProfile.nextProtos) {
		t.Fatalf("unexpected media ALPN list: %v", got)
	}
}

func TestNewUTLSTransportProfiles(t *testing.T) {
	mediaTransport := newUTLSTransport(nil, mediaUTLSProfile, nil)
	if mediaTransport.DialTLSContext == nil {
		t.Fatal("media transport should install a uTLS dialer")
	}
	if !mediaTransport.ForceAttemptHTTP2 {
		t.Fatal("media transport should keep HTTP/2 enabled")
	}
	if got := mediaTransport.TLSClientConfig.NextProtos; len(got) != 2 || got[0] != "h2" || got[1] != "http/1.1" {
		t.Fatalf("unexpected media ALPN list: %v", got)
	}

	websocketTransport := newUTLSTransport(nil, websocketUTLSProfile, nil)
	if websocketTransport.DialTLSContext == nil {
		t.Fatal("websocket transport should install a uTLS dialer")
	}
	if websocketTransport.ForceAttemptHTTP2 {
		t.Fatal("websocket transport should stay on HTTP/1.1")
	}
	if got := websocketTransport.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("unexpected websocket ALPN list: %v", got)
	}
}

func TestNewClientDoesNotEnableUTLSByDefault(t *testing.T) {
	cli := NewClient(nil, nil)

	if _, ok := cli.mediaHTTP.Transport.(*http.Transport); !ok {
		t.Fatalf("expected default media transport to stay plain http.Transport, got %T", cli.mediaHTTP.Transport)
	}
	if _, ok := cli.websocketHTTP.Transport.(*http.Transport); !ok {
		t.Fatalf("expected default websocket transport to stay plain http.Transport, got %T", cli.websocketHTTP.Transport)
	}
	if _, ok := cli.preLoginHTTP.Transport.(*http.Transport); !ok {
		t.Fatalf("expected default prelogin transport to stay plain http.Transport, got %T", cli.preLoginHTTP.Transport)
	}
}

func TestNewUTLSTransportHTTPSRequest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.TLSClientConfig = &gotls.Config{
		RootCAs:            rootCAs,
		InsecureSkipVerify: true,
	}

	client := &http.Client{
		Transport: newUTLSTransport(baseTransport, mediaUTLSProfile, nil),
	}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("uTLS transport request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected response body: %q", string(body))
	}
}

func TestWrapManagedMediaTransportHTTP2Request(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Fatalf("expected HTTP/2 request, got %s", r.Proto)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.TLSClientConfig = &gotls.Config{
		RootCAs:            rootCAs,
		InsecureSkipVerify: true,
	}

	raw, err := ParseUTLSClientHelloHex(sampleClientHelloHex)
	if err != nil {
		t.Fatalf("failed to parse sample ClientHello hex: %v", err)
	}

	client := &http.Client{
		Transport: wrapManagedMediaTransport(baseTransport, raw),
	}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("managed media transport request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected response body: %q", string(body))
	}
}

func getALPNProtocols(spec *utls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		alpnExt, ok := ext.(*utls.ALPNExtension)
		if ok {
			return append([]string(nil), alpnExt.AlpnProtocols...)
		}
	}
	return nil
}
