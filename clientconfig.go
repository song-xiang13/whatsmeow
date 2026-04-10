package whatsmeow

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/song-xiang13/whatsmeow/store"
	waLog "github.com/song-xiang13/whatsmeow/util/log"
)

// ClientConfig is a minimal file-friendly config for wiring a client with payload,
// request-header and TLS fingerprint overrides.
type ClientConfig struct {
	Payload        *BrowserTSExtractedConfig `json:"payload,omitempty"`
	RequestHeaders AllowedRequestHeaders     `json:"requestHeaders,omitempty"`
	TLS            ClientConfigTLS           `json:"tls,omitempty"`
}

type ClientConfigTLS struct {
	ClientHelloHex string `json:"clientHelloHex,omitempty"`
}

type rawBrowserInfoJSON struct {
	Version               BrowserTSVersionInfo              `json:"version"`
	UA                    BrowserTSUAInfo                   `json:"ua"`
	UserAgentData         BrowserTSUserAgentData            `json:"userAgentData"`
	Locale                BrowserTSLocaleInfo               `json:"locale"`
	DeviceInfoFromBackend *BrowserTSBackendDeviceInfoResult `json:"deviceInfoFromBackend,omitempty"`
	RegistrationPayload   *rawRegistrationPayload           `json:"registrationPayload,omitempty"`
}

type rawRegistrationPayload struct {
	UserAgentAppVersion *BrowserTSAppVersion `json:"userAgentAppVersion,omitempty"`
	DevicePropsVersion  *BrowserTSAppVersion `json:"devicePropsVersion,omitempty"`
	DevicePropsOS       string               `json:"devicePropsOs,omitempty"`
	DevicePropsPlatform int32                `json:"devicePropsPlatformType,omitempty"`
}

// LoadClientConfigFile reads a JSON config file from disk.
func LoadClientConfigFile(path string) (ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ClientConfig{}, err
	}
	return ParseClientConfig(data)
}

// ParseClientConfig parses a unified whatsmeow client config JSON blob.
func ParseClientConfig(data []byte) (ClientConfig, error) {
	var cfg ClientConfig
	if err := json.Unmarshal(data, &cfg); err == nil && cfg.hasConfigData() {
		return cfg, nil
	}
	return ClientConfig{}, fmt.Errorf("json file doesn't look like a supported whatsmeow client config")
}

// Options builds client options from the config.
func (cfg ClientConfig) Options() ([]ClientOption, error) {
	var opts []ClientOption

	payloadCfg, hasPayload, err := cfg.resolvePayloadConfig()
	if err != nil {
		return nil, err
	}
	if hasPayload {
		opts = append(opts, WithClientPayloadConfig(payloadCfg))
	}

	if hasRequestHeaderOverrides(cfg.RequestHeaders) {
		opts = append(opts, WithAllowedRequestHeaders(cfg.RequestHeaders))
	}

	record, err := cfg.resolveTLSClientHelloRecord()
	if err != nil {
		return nil, err
	}
	if len(record) > 0 {
		opts = append(opts, WithUTLSClientHelloRecord(record))
	}

	return opts, nil
}

// NewClientWithConfig creates a client directly from a parsed config.
func NewClientWithConfig(deviceStore *store.Device, log waLog.Logger, cfg ClientConfig) (*Client, error) {
	opts, err := cfg.Options()
	if err != nil {
		return nil, err
	}
	return NewClient(deviceStore, log, opts...), nil
}

// NewClientWithConfigFile creates a client from a JSON config file.
func NewClientWithConfigFile(deviceStore *store.Device, log waLog.Logger, path string) (*Client, error) {
	cfg, err := LoadClientConfigFile(path)
	if err != nil {
		return nil, err
	}
	return NewClientWithConfig(deviceStore, log, cfg)
}

// ParseClientPayloadConfig parses either the flattened BrowserTSExtractedConfig shape
// or the raw JSON shape produced by GetBrowserInfo.js.
func ParseClientPayloadConfig(data []byte) (ClientPayloadConfig, error) {
	var direct BrowserTSExtractedConfig
	if err := json.Unmarshal(data, &direct); err == nil && hasBrowserTSConfigData(direct) {
		return ClientPayloadConfigFromBrowserTSExtractedConfig(direct)
	}

	var raw rawBrowserInfoJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return ClientPayloadConfig{}, err
	}

	extracted := BrowserTSExtractedConfig{
		Version:               raw.Version,
		UA:                    raw.UA,
		UserAgentData:         raw.UserAgentData,
		Locale:                raw.Locale,
		DeviceInfoFromBackend: raw.DeviceInfoFromBackend,
	}
	if raw.RegistrationPayload != nil {
		extracted.DevicePropsVersion = raw.RegistrationPayload.DevicePropsVersion
		if extracted.UA.OS == "" {
			extracted.UA.OS = raw.RegistrationPayload.DevicePropsOS
		}
	}

	if !hasBrowserTSConfigData(extracted) {
		return ClientPayloadConfig{}, fmt.Errorf("json file doesn't look like a supported browser-ts config")
	}
	return ClientPayloadConfigFromBrowserTSExtractedConfig(extracted)
}

func (cfg ClientConfig) hasConfigData() bool {
	return cfg.Payload != nil ||
		hasRequestHeaderOverrides(cfg.RequestHeaders) ||
		strings.TrimSpace(cfg.TLS.ClientHelloHex) != ""
}

func (cfg ClientConfig) resolvePayloadConfig() (ClientPayloadConfig, bool, error) {
	if cfg.Payload != nil {
		payloadCfg, err := ClientPayloadConfigFromBrowserTSExtractedConfig(*cfg.Payload)
		return payloadCfg, true, err
	}
	return ClientPayloadConfig{}, false, nil
}

func (cfg ClientConfig) resolveTLSClientHelloRecord() ([]byte, error) {
	if strings.TrimSpace(cfg.TLS.ClientHelloHex) != "" {
		return ParseUTLSClientHelloHex(cfg.TLS.ClientHelloHex)
	}
	return nil, nil
}

func hasBrowserTSConfigData(cfg BrowserTSExtractedConfig) bool {
	return strings.TrimSpace(cfg.Version.VersionBase) != "" ||
		strings.TrimSpace(cfg.UA.OS) != "" ||
		strings.TrimSpace(cfg.UserAgentData.BrowserName) != "" ||
		cfg.DeviceInfoFromBackend != nil ||
		cfg.DevicePropsVersion != nil
}

func hasRequestHeaderOverrides(headers AllowedRequestHeaders) bool {
	return len(headers.Websocket) > 0 || len(headers.Media) > 0
}
