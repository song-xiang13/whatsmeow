# whatsmeow
[![Go Reference](https://pkg.go.dev/badge/github.com/song-xiang13/whatsmeow.svg)](https://pkg.go.dev/github.com/song-xiang13/whatsmeow)

whatsmeow is a Go library for the WhatsApp web multidevice API.

## Discussion
Matrix room: [#whatsmeow:maunium.net](https://matrix.to/#/#whatsmeow:maunium.net)

For questions about the WhatsApp protocol (like how to send a specific type of
message), you can also use the [WhatsApp protocol Q&A] section on GitHub
discussions.

[WhatsApp protocol Q&A]: https://github.com/tulir/whatsmeow/discussions/categories/whatsapp-protocol-q-a

## Usage
The [godoc](https://pkg.go.dev/github.com/song-xiang13/whatsmeow) includes docs for all methods and event types.
There's also a [simple example](https://pkg.go.dev/github.com/song-xiang13/whatsmeow#example-package) at the top.

### Client Config

whatsmeow can optionally load client fingerprint overrides from a single JSON file.

Use the regular constructor when you want the default behavior:

```go
client := whatsmeow.NewClient(deviceStore, log)
```

Use the config-based constructor when you want per-client payload, request header or TLS fingerprint overrides:

```go
client, err := whatsmeow.NewClientWithConfigFile(deviceStore, log, "whatsmeow.config.json")
if err != nil {
	panic(err)
}
```

The config file is a single JSON document with three optional sections:

```json
{
  "payload": {
    "version": {
      "VERSION_BASE": "2.3000.1036426764"
    },
    "ua": {
      "os": "Windows",
      "osVersion": "10",
      "browser": "chrome"
    }
  },
  "requestHeaders": {
    "websocket": {
      "Accept-Language": ["zh-CN,zh;q=0.9"]
    },
    "media": {
      "Cache-Control": ["no-cache"]
    }
  },
  "tls": {
    "clientHelloHex": "1603010200..."
  }
}
```

Notes:

* `payload` is the browser payload object itself.
* `requestHeaders` only supports a small allowlist: `Accept-Language`, `Cache-Control`, `Pragma`.
* `tls.clientHelloHex` is optional. If it is omitted, whatsmeow keeps the default transport behavior.
* Any section can be omitted. Unset values fall back to the library defaults.
* A full example file is available at `whatsmeow.config.sample.json`.

The example entrypoints also accept `-config <path>` and will use `NewClientWithConfigFile(...)` when provided.

### Extracting Browser Data

`GetBrowserInfo.js` is included to help extract the browser-side data used by the `payload` section.

Typical flow:

1. Open WhatsApp Web in the browser you want to mirror.
2. Run `GetBrowserInfo.js` in the browser console.
3. Copy the relevant values into `whatsmeow.config.json` under `payload`.

The script output matches the `payload` object shape used by whatsmeow. In practice, you can copy the returned object into `whatsmeow.config.json` as the value of `payload`.

Field mapping:

* Copy `version`, `ua`, `userAgentData`, `locale`, `devicePropsVersion` and `deviceInfoFromBackend` into `payload`.

## Features
Most core features are already present:

* Sending messages to private chats and groups (both text and media)
* Receiving all messages
* Managing groups and receiving group change events
* Joining via invite messages, using and creating invite links
* Sending and receiving typing notifications
* Sending and receiving delivery and read receipts
* Reading and writing app state (contact list, chat pin/mute status, etc)
* Sending and handling retry receipts if message decryption fails
* Sending status messages (experimental, may not work for large contact lists)

Things that are not yet implemented:

* Sending broadcast list messages (this is not supported on WhatsApp web either)
* Calls
