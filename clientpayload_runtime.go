// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import "github.com/song-xiang13/whatsmeow/proto/waWa6"

func (cli *Client) getCurrentClientPayload() *waWa6.ClientPayload {
	if cli == nil {
		return nil
	}
	if cli.GetClientPayload != nil {
		return cli.GetClientPayload()
	}
	if cli.Store == nil {
		return nil
	}
	return cli.Store.GetClientPayload()
}
