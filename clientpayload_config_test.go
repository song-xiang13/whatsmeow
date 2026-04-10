package whatsmeow

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/song-xiang13/whatsmeow/proto/waCompanionReg"
	"github.com/song-xiang13/whatsmeow/proto/waWa6"
	"github.com/song-xiang13/whatsmeow/store"
	"github.com/song-xiang13/whatsmeow/types"
	"github.com/song-xiang13/whatsmeow/util/keys"
)

func browserTSExtractedConfigData() BrowserTSExtractedConfig {
	return BrowserTSExtractedConfig{
		Version: BrowserTSVersionInfo{
			VersionBase: "2.3000.1036426764",
		},
		UA: BrowserTSUAInfo{
			OS:        "Windows",
			OSVersion: "10",
			Browser:   "chrome",
		},
		UserAgentData: BrowserTSUserAgentData{
			BrowserName:         "Chrome",
			BrowserFullVersion:  "146.0.0.0",
			PlatformName:        "Windows",
			PlatformFullVersion: "10",
			DeviceName:          "Unknown",
		},
		Locale: BrowserTSLocaleInfo{
			LocaleLanguageIso6391:       "zh",
			LocaleCountryIso31661Alpha2: "CN",
		},
		DevicePropsVersion: &BrowserTSAppVersion{
			Primary: 10,
		},
		DeviceInfoFromBackend: &BrowserTSBackendDeviceInfoResult{
			Raw: &BrowserTSBackendDeviceInfo{
				OSVersion:    "0.1",
				OSBuild:      "0.1",
				Hardware:     "desktop",
				Manufacturer: "",
				Device:       "Desktop",
				LanguageCode: "zh",
				LocationCode: "US",
				MCC:          "000",
				MNC:          "000",
			},
			LanguageCode:  "zh",
			LocationCode:  "US",
			OSBuildNumber: "0.1",
			MCC:           "000",
			MNC:           "000",
		},
	}
}

func browserTSExtractedConfig(t *testing.T) ClientPayloadConfig {
	t.Helper()

	cfg, err := ClientPayloadConfigFromBrowserTSExtractedConfig(browserTSExtractedConfigData())
	if err != nil {
		t.Fatalf("failed to convert browser-ts extracted config: %v", err)
	}
	return cfg
}

func TestBrowserTSExtractedConfigLoginPayload(t *testing.T) {
	cfg := browserTSExtractedConfig(t)

	jid := types.NewJID("123456789", types.DefaultUserServer)
	device := &store.Device{ID: &jid}
	payload := cfg.GetClientPayload(device)

	if got := payload.GetUserAgent().GetAppVersion().GetTertiary(); got != 1036426764 {
		t.Fatalf("unexpected app version tertiary: got %d", got)
	}
	if got := payload.GetUserAgent().GetMcc(); got != "000" {
		t.Fatalf("unexpected mcc: got %q", got)
	}
	if got := payload.GetUserAgent().GetMnc(); got != "000" {
		t.Fatalf("unexpected mnc: got %q", got)
	}
	if got := payload.GetUserAgent().GetOsVersion(); got != "0.1" {
		t.Fatalf("unexpected os version: got %q", got)
	}
	if got := payload.GetUserAgent().GetOsBuildNumber(); got != "0.1" {
		t.Fatalf("unexpected os build number: got %q", got)
	}
	if got := payload.GetUserAgent().GetDevice(); got != "Desktop" {
		t.Fatalf("unexpected device: got %q", got)
	}
	if got := payload.GetUserAgent().GetLocaleLanguageIso6391(); got != "zh" {
		t.Fatalf("unexpected locale language: got %q", got)
	}
	if got := payload.GetUserAgent().GetLocaleCountryIso31661Alpha2(); got != "CN" {
		t.Fatalf("unexpected locale country: got %q", got)
	}
	if payload.GetDevicePairingData() != nil {
		t.Fatalf("login payload should not include pairing data")
	}
}

func TestBrowserTSExtractedConfigRegistrationPayload(t *testing.T) {
	cfg := browserTSExtractedConfig(t)

	identityKey := keys.NewKeyPair()
	device := &store.Device{
		IdentityKey:    identityKey,
		SignedPreKey:   identityKey.CreateSignedPreKey(7),
		RegistrationID: 99,
	}

	payload := cfg.GetClientPayload(device)
	expectedHash := cfg.WAVersion.Hash()
	if !bytes.Equal(payload.GetDevicePairingData().GetBuildHash(), expectedHash[:]) {
		t.Fatalf("unexpected build hash for custom version")
	}

	var deviceProps waCompanionReg.DeviceProps
	if err := proto.Unmarshal(payload.GetDevicePairingData().GetDeviceProps(), &deviceProps); err != nil {
		t.Fatalf("failed to unmarshal device props: %v", err)
	}
	if got := deviceProps.GetOs(); got != "Windows" {
		t.Fatalf("unexpected device props os: got %q", got)
	}
	if got := deviceProps.GetVersion().GetPrimary(); got != 10 {
		t.Fatalf("unexpected device props version primary: got %d", got)
	}
	if got := deviceProps.GetVersion().GetSecondary(); got != 0 {
		t.Fatalf("unexpected device props version secondary: got %d", got)
	}
	if got := deviceProps.GetPlatformType(); got != waCompanionReg.DeviceProps_CHROME {
		t.Fatalf("unexpected device props platform type: got %v", got)
	}
}

func TestWithClientPayloadConfigUsesBrowserTSExtractedConfig(t *testing.T) {
	cfg := browserTSExtractedConfig(t)

	jid := types.NewJID("123456789", types.DefaultUserServer)
	deviceStore := &store.Device{ID: &jid}
	cli := NewClient(deviceStore, nil, WithClientPayloadConfig(cfg))

	if cli.GetClientPayload == nil {
		t.Fatalf("expected custom payload getter to be configured")
	}

	payload := cli.GetClientPayload()
	if got := payload.GetUserAgent().GetAppVersion().GetTertiary(); got != 1036426764 {
		t.Fatalf("unexpected app version tertiary: got %d", got)
	}
	if got := payload.GetUserAgent().GetLocaleLanguageIso6391(); got != "zh" {
		t.Fatalf("unexpected locale language: got %q", got)
	}
	if !payload.GetPassive() {
		t.Fatalf("expected login payload to keep legacy passive=true by default")
	}
}

func TestConvertQueryIDUsesEffectiveClientPayload(t *testing.T) {
	cfg := DefaultClientPayloadConfig()
	cfg.BaseClientPayload.UserAgent.Platform = waWa6.ClientPayload_UserAgent_MACOS.Enum()

	jid := types.NewJID("123456789", types.DefaultUserServer)
	cli := NewClient(&store.Device{ID: &jid}, nil, WithClientPayloadConfig(cfg))
	got := convertQueryID(cli, queryFetchNewsletter)
	if got != queryFetchNewsletterDesktop {
		t.Fatalf("unexpected query id: got %q want %q", got, queryFetchNewsletterDesktop)
	}
}

func TestSendMexIQUsesEffectiveClientPayload(t *testing.T) {
	cfg := DefaultClientPayloadConfig()
	cfg.BaseClientPayload.UserAgent.Platform = waWa6.ClientPayload_UserAgent_MACOS.Enum()

	jid := types.NewJID("123456789", types.DefaultUserServer)
	cli := NewClient(&store.Device{ID: &jid}, nil, WithClientPayloadConfig(cfg))
	_, err := cli.sendMexIQ(context.Background(), queryFetchNewsletter, map[string]any{})
	if err == nil || err.Error() != "argo decoding is currently broken" {
		t.Fatalf("unexpected sendMexIQ result: %v", err)
	}
}
