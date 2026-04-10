package whatsmeow

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/song-xiang13/whatsmeow/store"
	"github.com/song-xiang13/whatsmeow/types"
)

func TestParseClientConfigAcceptsPayloadOnlyJSON(t *testing.T) {
	extracted := browserTSExtractedConfigData()
	data, err := json.Marshal(ClientConfig{
		Payload: &extracted,
	})
	if err != nil {
		t.Fatalf("marshal client config: %v", err)
	}

	cfg, err := ParseClientConfig(data)
	if err != nil {
		t.Fatalf("parse client config: %v", err)
	}

	jid := types.NewJID("123456789", types.DefaultUserServer)
	cli, err := NewClientWithConfig(&store.Device{ID: &jid}, nil, cfg)
	if err != nil {
		t.Fatalf("new client with config: %v", err)
	}
	if cli.GetClientPayload == nil {
		t.Fatal("expected payload override to be installed")
	}

	payload := cli.GetClientPayload()
	if got := payload.GetUserAgent().GetLocaleLanguageIso6391(); got != "zh" {
		t.Fatalf("unexpected locale language: got %q", got)
	}
}

func TestLoadClientConfigFileUsesSingleJSON(t *testing.T) {
	dir := t.TempDir()
	extracted := browserTSExtractedConfigData()
	configData, err := json.Marshal(ClientConfig{
		Payload: &extracted,
		RequestHeaders: AllowedRequestHeaders{
			Websocket: http.Header{
				"Accept-Language": {"zh-CN,zh;q=0.9"},
			},
			Media: http.Header{
				"Cache-Control": {"no-cache"},
			},
		},
		TLS: ClientConfigTLS{
			ClientHelloHex: sampleClientHelloHex,
		},
	})
	if err != nil {
		t.Fatalf("marshal client config: %v", err)
	}

	configPath := dir + string(os.PathSeparator) + "whatsmeow.config.json"
	if err = os.WriteFile(configPath, configData, 0o644); err != nil {
		t.Fatalf("write client config: %v", err)
	}

	jid := types.NewJID("123456789", types.DefaultUserServer)
	cli, err := NewClientWithConfigFile(&store.Device{ID: &jid}, nil, configPath)
	if err != nil {
		t.Fatalf("new client with config file: %v", err)
	}

	if got := cli.websocketHeaderOverrides.Get("Accept-Language"); got != "zh-CN,zh;q=0.9" {
		t.Fatalf("unexpected websocket Accept-Language: got %q", got)
	}
	if got := cli.mediaHeaderOverrides.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unexpected media Cache-Control: got %q", got)
	}
	if len(cli.utlsClientHelloRecord) == 0 {
		t.Fatal("expected tls clienthello override to be loaded")
	}
	if cli.GetClientPayload == nil {
		t.Fatal("expected payload override to be loaded")
	}
}
