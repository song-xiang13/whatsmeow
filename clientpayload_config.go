// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/libsignal/ecc"

	"github.com/song-xiang13/whatsmeow/proto/waCompanionReg"
	"github.com/song-xiang13/whatsmeow/proto/waWa6"
	"github.com/song-xiang13/whatsmeow/store"
	"github.com/song-xiang13/whatsmeow/types"
)

// ClientPayloadConfig contains per-client overrides for the handshake payload.
//
// If a field is unset, the current global defaults in the store package are used.
type ClientPayloadConfig struct {
	WAVersion         store.WAVersionContainer
	BaseClientPayload *waWa6.ClientPayload
	DeviceProps       *waCompanionReg.DeviceProps
}

// DefaultClientPayloadConfig returns a deep copy of the current global payload defaults.
func DefaultClientPayloadConfig() ClientPayloadConfig {
	return ClientPayloadConfig{
		WAVersion:         store.GetWAVersion(),
		BaseClientPayload: proto.Clone(store.BaseClientPayload).(*waWa6.ClientPayload),
		DeviceProps:       proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps),
	}
}

// Clone deep-copies the config so it can be reused safely across clients.
func (cfg ClientPayloadConfig) Clone() ClientPayloadConfig {
	cloned := ClientPayloadConfig{WAVersion: cfg.WAVersion}
	if cfg.BaseClientPayload != nil {
		cloned.BaseClientPayload = proto.Clone(cfg.BaseClientPayload).(*waWa6.ClientPayload)
	}
	if cfg.DeviceProps != nil {
		cloned.DeviceProps = proto.Clone(cfg.DeviceProps).(*waCompanionReg.DeviceProps)
	}
	return cloned
}

func appVersionToWAVersion(appVersion *waWa6.ClientPayload_UserAgent_AppVersion) store.WAVersionContainer {
	if appVersion == nil {
		return store.WAVersionContainer{}
	}
	return store.WAVersionContainer{
		appVersion.GetPrimary(),
		appVersion.GetSecondary(),
		appVersion.GetTertiary(),
	}
}

func (cfg ClientPayloadConfig) resolvedWAVersion() store.WAVersionContainer {
	if !cfg.WAVersion.IsZero() {
		return cfg.WAVersion
	}
	if cfg.BaseClientPayload != nil {
		if version := appVersionToWAVersion(cfg.BaseClientPayload.GetUserAgent().GetAppVersion()); !version.IsZero() {
			return version
		}
	}
	return store.GetWAVersion()
}

func (cfg ClientPayloadConfig) cloneBaseClientPayload(version store.WAVersionContainer) *waWa6.ClientPayload {
	var payload *waWa6.ClientPayload
	if cfg.BaseClientPayload != nil {
		payload = proto.Clone(cfg.BaseClientPayload).(*waWa6.ClientPayload)
	} else {
		payload = proto.Clone(store.BaseClientPayload).(*waWa6.ClientPayload)
	}
	if payload.UserAgent == nil {
		payload.UserAgent = &waWa6.ClientPayload_UserAgent{}
	}
	payload.UserAgent.AppVersion = version.ProtoAppVersion()
	return payload
}

func (cfg ClientPayloadConfig) cloneDeviceProps() *waCompanionReg.DeviceProps {
	if cfg.DeviceProps != nil {
		return proto.Clone(cfg.DeviceProps).(*waCompanionReg.DeviceProps)
	}
	return proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
}

func (cfg ClientPayloadConfig) getRegistrationPayload(device *store.Device) *waWa6.ClientPayload {
	version := cfg.resolvedWAVersion()
	payload := cfg.cloneBaseClientPayload(version)
	regID := make([]byte, 4)
	binary.BigEndian.PutUint32(regID, device.RegistrationID)
	preKeyID := make([]byte, 4)
	binary.BigEndian.PutUint32(preKeyID, device.SignedPreKey.KeyID)
	deviceProps, _ := proto.Marshal(cfg.cloneDeviceProps())
	buildHash := version.Hash()
	payload.DevicePairingData = &waWa6.ClientPayload_DevicePairingRegistrationData{
		ERegid:      regID,
		EKeytype:    []byte{ecc.DjbType},
		EIdent:      device.IdentityKey.Pub[:],
		ESkeyID:     preKeyID[1:],
		ESkeyVal:    device.SignedPreKey.Pub[:],
		ESkeySig:    device.SignedPreKey.Signature[:],
		BuildHash:   buildHash[:],
		DeviceProps: deviceProps,
	}
	payload.Passive = proto.Bool(false)
	payload.Pull = proto.Bool(false)
	return payload
}

func (cfg ClientPayloadConfig) getLoginPayload(device *store.Device) *waWa6.ClientPayload {
	payload := cfg.cloneBaseClientPayload(cfg.resolvedWAVersion())
	payload.Username = proto.Uint64(device.ID.UserInt())
	payload.Device = proto.Uint32(uint32(device.ID.Device))
	if payload.Passive == nil {
		payload.Passive = proto.Bool(true)
	}
	payload.Pull = proto.Bool(true)
	payload.LidDbMigrated = proto.Bool(true)
	if payload.Lc == nil {
		payload.Lc = proto.Int32(1)
	}
	return payload
}

// GetClientPayload builds a client payload for the given device using this config.
func (cfg ClientPayloadConfig) GetClientPayload(device *store.Device) *waWa6.ClientPayload {
	if device.ID != nil {
		if *device.ID == types.EmptyJID {
			panic(fmt.Errorf("GetClientPayload called with empty JID"))
		}
		return cfg.getLoginPayload(device)
	}
	return cfg.getRegistrationPayload(device)
}
