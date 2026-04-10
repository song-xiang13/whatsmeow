// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/song-xiang13/whatsmeow/proto/waCompanionReg"
	"github.com/song-xiang13/whatsmeow/proto/waWa6"
	"github.com/song-xiang13/whatsmeow/store"
)

// BrowserTSExtractedConfig mirrors the structure produced by the browser-ts extraction script.
type BrowserTSExtractedConfig struct {
	Version               BrowserTSVersionInfo              `json:"version"`
	UA                    BrowserTSUAInfo                   `json:"ua"`
	UserAgentData         BrowserTSUserAgentData            `json:"userAgentData"`
	Locale                BrowserTSLocaleInfo               `json:"locale"`
	DevicePropsVersion    *BrowserTSAppVersion              `json:"devicePropsVersion,omitempty"`
	DeviceInfoFromBackend *BrowserTSBackendDeviceInfoResult `json:"deviceInfoFromBackend,omitempty"`
}

type BrowserTSVersionInfo struct {
	VersionBase string `json:"VERSION_BASE"`
}

type BrowserTSUAInfo struct {
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"`
	Browser   string `json:"browser"`
}

type BrowserTSUserAgentData struct {
	BrowserName         string `json:"browserName"`
	BrowserFullVersion  string `json:"browserFullVersion"`
	PlatformName        string `json:"platformName"`
	PlatformFullVersion string `json:"platformFullVersion"`
	DeviceName          string `json:"deviceName"`
}

type BrowserTSAppVersion struct {
	Primary    uint32 `json:"primary,omitempty"`
	Secondary  uint32 `json:"secondary,omitempty"`
	Tertiary   uint32 `json:"tertiary,omitempty"`
	Quaternary uint32 `json:"quaternary,omitempty"`
	Quinary    uint32 `json:"quinary,omitempty"`
}

type BrowserTSLocaleInfo struct {
	LocaleLanguageIso6391       string `json:"localeLanguageIso6391"`
	LocaleCountryIso31661Alpha2 string `json:"localeCountryIso31661Alpha2"`
}

type BrowserTSBackendDeviceInfoResult struct {
	Raw           *BrowserTSBackendDeviceInfo `json:"raw,omitempty"`
	LanguageCode  string                      `json:"languageCode"`
	LocationCode  string                      `json:"locationCode"`
	OSBuildNumber string                      `json:"osBuildNumber"`
	MCC           string                      `json:"mcc"`
	MNC           string                      `json:"mnc"`
}

type BrowserTSBackendDeviceInfo struct {
	OSVersion    string `json:"osVersion"`
	OSBuild      string `json:"osBuild"`
	Hardware     string `json:"hardware"`
	Manufacturer string `json:"manufacturer"`
	Device       string `json:"device"`
	LanguageCode string `json:"lg"`
	LocationCode string `json:"lc"`
	MCC          string `json:"mcc"`
	MNC          string `json:"mnc"`
}

// ClientPayloadConfigFromBrowserTSExtractedConfig builds a payload config from browser-ts extracted values.
func ClientPayloadConfigFromBrowserTSExtractedConfig(extracted BrowserTSExtractedConfig) (ClientPayloadConfig, error) {
	cfg := DefaultClientPayloadConfig()
	return cfg, cfg.ApplyBrowserTSExtractedConfig(extracted)
}

// ApplyBrowserTSExtractedConfig applies browser-ts extracted values on top of an existing config.
func (cfg *ClientPayloadConfig) ApplyBrowserTSExtractedConfig(extracted BrowserTSExtractedConfig) error {
	if cfg.BaseClientPayload == nil {
		cfg.BaseClientPayload = proto.Clone(store.BaseClientPayload).(*waWa6.ClientPayload)
	}
	if cfg.DeviceProps == nil {
		cfg.DeviceProps = proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
	}
	if cfg.BaseClientPayload.UserAgent == nil {
		cfg.BaseClientPayload.UserAgent = &waWa6.ClientPayload_UserAgent{}
	}

	if versionBase := strings.TrimSpace(extracted.Version.VersionBase); versionBase != "" {
		parsed, err := store.ParseVersion(versionBase)
		if err != nil {
			return fmt.Errorf("failed to parse browser-ts version %q: %w", versionBase, err)
		}
		cfg.WAVersion = parsed
		cfg.BaseClientPayload.UserAgent.AppVersion = parsed.ProtoAppVersion()
	}

	if info := extracted.DeviceInfoFromBackend; info != nil {
		if raw := info.Raw; raw != nil {
			setStringPtr(&cfg.BaseClientPayload.UserAgent.Mcc, raw.MCC)
			setStringPtr(&cfg.BaseClientPayload.UserAgent.Mnc, raw.MNC)
			setStringPtr(&cfg.BaseClientPayload.UserAgent.OsVersion, raw.OSVersion)
			setStringPtr(&cfg.BaseClientPayload.UserAgent.OsBuildNumber, raw.OSBuild)
			setStringPtr(&cfg.BaseClientPayload.UserAgent.Manufacturer, raw.Manufacturer)
			setStringPtr(&cfg.BaseClientPayload.UserAgent.Device, raw.Device)
		}
		setStringPtr(&cfg.BaseClientPayload.UserAgent.Mcc, info.MCC)
		setStringPtr(&cfg.BaseClientPayload.UserAgent.Mnc, info.MNC)
		if cfg.BaseClientPayload.UserAgent.GetOsBuildNumber() == "" {
			setStringPtr(&cfg.BaseClientPayload.UserAgent.OsBuildNumber, info.OSBuildNumber)
		}
	}

	setStringPtr(&cfg.BaseClientPayload.UserAgent.LocaleLanguageIso6391, extracted.Locale.LocaleLanguageIso6391)
	setStringPtr(&cfg.BaseClientPayload.UserAgent.LocaleCountryIso31661Alpha2, extracted.Locale.LocaleCountryIso31661Alpha2)

	if normalizedOS := normalizeBrowserTSOS(extracted); normalizedOS != "" {
		setStringPtr(&cfg.DeviceProps.Os, normalizedOS)
	}
	if version := browserTSDevicePropsVersion(extracted); version != nil {
		cfg.DeviceProps.Version = version
	}
	if platformType := browserTSPlatformType(extracted); platformType != waCompanionReg.DeviceProps_UNKNOWN {
		cfg.DeviceProps.PlatformType = platformType.Enum()
	}
	return nil
}

func setStringPtr(target **string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	*target = proto.String(value)
}

func normalizeBrowserTSOS(extracted BrowserTSExtractedConfig) string {
	if osName := strings.TrimSpace(extracted.UA.OS); osName != "" {
		return osName
	}
	return strings.TrimSpace(extracted.UserAgentData.PlatformName)
}

func browserTSPlatformType(extracted BrowserTSExtractedConfig) waCompanionReg.DeviceProps_PlatformType {
	browserName := strings.ToLower(strings.TrimSpace(extracted.UserAgentData.BrowserName))
	if browserName == "" {
		browserName = strings.ToLower(strings.TrimSpace(extracted.UA.Browser))
	}
	switch browserName {
	case "chrome", "chromium":
		return waCompanionReg.DeviceProps_CHROME
	case "firefox":
		return waCompanionReg.DeviceProps_FIREFOX
	case "safari":
		return waCompanionReg.DeviceProps_SAFARI
	case "edge", "edg":
		return waCompanionReg.DeviceProps_EDGE
	case "opera":
		return waCompanionReg.DeviceProps_OPERA
	case "ie", "internet explorer", "trident":
		return waCompanionReg.DeviceProps_IE
	default:
		deviceName := strings.ToLower(strings.TrimSpace(extracted.UserAgentData.DeviceName))
		platformName := strings.ToLower(strings.TrimSpace(extracted.UserAgentData.PlatformName))
		if deviceName == "desktop" || deviceName == "unknown" || platformName == "windows" || platformName == "mac os" || platformName == "chromium os" {
			return waCompanionReg.DeviceProps_DESKTOP
		}
		return waCompanionReg.DeviceProps_UNKNOWN
	}
}

func browserTSDevicePropsVersion(extracted BrowserTSExtractedConfig) *waCompanionReg.DeviceProps_AppVersion {
	if extracted.DevicePropsVersion != nil {
		if version := extracted.DevicePropsVersion.proto(); version != nil {
			return version
		}
	}

	for _, candidate := range []string{
		strings.TrimSpace(extracted.UserAgentData.PlatformFullVersion),
		strings.TrimSpace(extracted.UA.OSVersion),
	} {
		if version := parseBrowserTSAppVersion(candidate); version != nil {
			return version
		}
	}

	return nil
}

func parseBrowserTSAppVersion(version string) *waCompanionReg.DeviceProps_AppVersion {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}

	parts := strings.Split(version, ".")
	if len(parts) > 5 {
		parts = parts[:5]
	}

	parsed := make([]uint32, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			break
		}

		var value uint64
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil
			}
			value = value*10 + uint64(r-'0')
		}
		parsed = append(parsed, uint32(value))
	}
	if len(parsed) == 0 {
		return nil
	}

	versionProto := &waCompanionReg.DeviceProps_AppVersion{
		Primary: proto.Uint32(parsed[0]),
	}
	if len(parsed) > 1 {
		versionProto.Secondary = proto.Uint32(parsed[1])
	}
	if len(parsed) > 2 {
		versionProto.Tertiary = proto.Uint32(parsed[2])
	}
	if len(parsed) > 3 {
		versionProto.Quaternary = proto.Uint32(parsed[3])
	}
	if len(parsed) > 4 {
		versionProto.Quinary = proto.Uint32(parsed[4])
	}
	return versionProto
}

func (v *BrowserTSAppVersion) proto() *waCompanionReg.DeviceProps_AppVersion {
	if v == nil {
		return nil
	}
	if v.Primary == 0 && v.Secondary == 0 && v.Tertiary == 0 && v.Quaternary == 0 && v.Quinary == 0 {
		return nil
	}

	protoVersion := &waCompanionReg.DeviceProps_AppVersion{}
	if v.Primary != 0 {
		protoVersion.Primary = proto.Uint32(v.Primary)
	}
	if v.Secondary != 0 {
		protoVersion.Secondary = proto.Uint32(v.Secondary)
	}
	if v.Tertiary != 0 {
		protoVersion.Tertiary = proto.Uint32(v.Tertiary)
	}
	if v.Quaternary != 0 {
		protoVersion.Quaternary = proto.Uint32(v.Quaternary)
	}
	if v.Quinary != 0 {
		protoVersion.Quinary = proto.Uint32(v.Quinary)
	}
	if protoVersion.Primary == nil {
		return nil
	}
	return protoVersion
}
