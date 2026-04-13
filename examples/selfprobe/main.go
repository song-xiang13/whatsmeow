package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/song-xiang13/whatsmeow"
	"github.com/song-xiang13/whatsmeow/store/sqlstore"
	"github.com/song-xiang13/whatsmeow/types/events"
	waLog "github.com/song-xiang13/whatsmeow/util/log"
)

const (
	newAccountQueryID       = "25808450525504568"
	reachoutTimelockQueryID = "23983697327930364"
	messageCappingQueryID   = "25275321118723031"
)

type newAccountResponse struct {
	Users []struct {
		ID                   string `json:"id"`
		IntegritySignalsInfo *struct {
			TypeName     string `json:"__typename"`
			IsNewAccount *bool  `json:"is_new_account"`
		} `json:"integrity_signals_info"`
	} `json:"xwa2_fetch_wa_users"`
}

type reachoutTimelockResponse struct {
	Data struct {
		IsActive            bool   `json:"is_active"`
		TimeEnforcementEnds any    `json:"time_enforcement_ends"`
		EnforcementType     string `json:"enforcement_type"`
	} `json:"xwa2_fetch_account_reachout_timelock"`
}

type queryInput struct {
	JID              string         `json:"jid"`
	IntegritySignals map[string]any `json:"integrity_signals"`
}

func main() {
	dbFlag := flag.String("db", "", "Path to the SQLite session database")
	configFlag := flag.String("config", "", "Optional client config JSON file")
	socks5Flag := flag.String("socks5", "", "Optional SOCKS5 proxy URL")
	timeoutFlag := flag.Duration("connect-timeout", 2*time.Minute, "Maximum time to wait for login")
	logLevelFlag := flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
	flag.Parse()

	if *dbFlag == "" {
		flag.Usage()
		os.Exit(2)
	}

	client, err := openClient(*dbFlag, *socks5Flag, *logLevelFlag, *configFlag)
	must(err, "open client")
	defer client.Disconnect()

	must(ensureConnected(client, *timeoutFlag), "connect WhatsApp client")

	internal := client.DangerousInternals()
	ownID := internal.GetOwnID()
	fmt.Printf("own_jid=%s\n", ownID.String())

	rawReachout, err := internal.SendMexIQ(context.Background(), reachoutTimelockQueryID, map[string]any{})
	must(err, "query reachout timelock")
	fmt.Printf("reachout_raw=%s\n", string(rawReachout))

	var reachout reachoutTimelockResponse
	must(json.Unmarshal(rawReachout, &reachout), "decode reachout timelock")
	fmt.Printf("reachout_active=%v enforcement_type=%s time_enforcement_ends=%v\n",
		reachout.Data.IsActive,
		reachout.Data.EnforcementType,
		reachout.Data.TimeEnforcementEnds,
	)

	rawNewAccount, err := internal.SendMexIQ(context.Background(), newAccountQueryID, map[string]any{
		"input": map[string]any{
			"query_input": []queryInput{{
				JID: ownID.String(),
				IntegritySignals: map[string]any{
					"use_case": "START_CHAT_CONTEXT",
				},
			}},
			"telemetry": map[string]any{
				"context": "INTERACTIVE",
			},
		},
	})
	must(err, "query new account signal")
	fmt.Printf("new_account_raw=%s\n", string(rawNewAccount))

	var newAccount newAccountResponse
	must(json.Unmarshal(rawNewAccount, &newAccount), "decode new account signal")
	if len(newAccount.Users) == 0 {
		fmt.Println("new_account_result=missing")
	} else {
		row := newAccount.Users[0]
		value := "null"
		typeName := ""
		if row.IntegritySignalsInfo != nil {
			typeName = row.IntegritySignalsInfo.TypeName
			if row.IntegritySignalsInfo.IsNewAccount != nil {
				value = fmt.Sprintf("%v", *row.IntegritySignalsInfo.IsNewAccount)
			}
		}
		fmt.Printf("new_account=%s typename=%s\n", value, typeName)
	}

	rawCapping, err := internal.SendMexIQ(context.Background(), messageCappingQueryID, map[string]any{})
	if err != nil {
		fmt.Printf("message_capping_error=%v\n", err)
		return
	}
	fmt.Printf("message_capping_raw=%s\n", string(rawCapping))
}

func openClient(dbPath, proxyAddr, logLevel, configPath string) (*whatsmeow.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dbLog := waLog.Stdout("Database", logLevel, true)
	container, err := sqlstore.New(ctx, "sqlite3", sqliteDSN(dbPath), dbLog)
	if err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device session: %w", err)
	}
	clientLog := waLog.Stdout("Client", logLevel, true)

	var client *whatsmeow.Client
	if strings.TrimSpace(configPath) != "" {
		client, err = whatsmeow.NewClientWithConfigFile(deviceStore, clientLog, configPath)
		if err != nil {
			return nil, fmt.Errorf("load client config: %w", err)
		}
	} else {
		client = whatsmeow.NewClient(deviceStore, clientLog)
	}
	if strings.TrimSpace(proxyAddr) != "" {
		if err = client.SetProxyAddress(proxyAddr); err != nil {
			return nil, fmt.Errorf("configure socks5 proxy: %w", err)
		}
	}
	return client, nil
}

func ensureConnected(client *whatsmeow.Client, timeout time.Duration) error {
	monitor := newConnectMonitor(client)
	if client.Store.ID == nil {
		return fmt.Errorf("no stored login session")
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect with stored session: %w", err)
	}
	return monitor.wait(timeout, client)
}

type connectMonitor struct {
	ready chan struct{}
	errCh chan error
}

func newConnectMonitor(client *whatsmeow.Client) *connectMonitor {
	monitor := &connectMonitor{
		ready: make(chan struct{}),
		errCh: make(chan error, 1),
	}
	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Connected:
			select {
			case <-monitor.ready:
			default:
				close(monitor.ready)
			}
		case *events.LoggedOut:
			monitor.fail(fmt.Errorf("session logged out during connect (reason: %s)", v.Reason))
		case *events.ConnectFailure:
			monitor.fail(fmt.Errorf("connect failure from WhatsApp: %s (%s)", v.Reason, v.Message))
		case *events.TemporaryBan:
			monitor.fail(fmt.Errorf("account temporarily banned: %s", v.String()))
		case *events.StreamReplaced:
			monitor.fail(fmt.Errorf("session was taken over by another client"))
		case *events.ClientOutdated:
			monitor.fail(fmt.Errorf("client version rejected by WhatsApp as outdated"))
		case *events.CATRefreshError:
			monitor.fail(fmt.Errorf("CAT refresh error: %v", v.Error))
		}
	})
	return monitor
}

func (m *connectMonitor) fail(err error) {
	select {
	case m.errCh <- err:
	default:
	}
}

func (m *connectMonitor) wait(timeout time.Duration, client *whatsmeow.Client) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		if client.WaitForConnection(250 * time.Millisecond) {
			return nil
		}
		select {
		case <-m.ready:
			if client.WaitForConnection(10 * time.Second) {
				return nil
			}
			return fmt.Errorf("connected event arrived, but login handshake did not finish")
		case err := <-m.errCh:
			return err
		case <-timer.C:
			return fmt.Errorf("client did not become ready within %s", timeout)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func sqliteDSN(dbPath string) string {
	normalized := filepath.ToSlash(dbPath)
	if strings.HasPrefix(normalized, "file:") {
		if strings.Contains(normalized, "?") {
			return normalized
		}
		return normalized + "?_foreign_keys=on"
	}
	if strings.Contains(normalized, "?") {
		return "file:" + normalized + "&_foreign_keys=on"
	}
	return "file:" + normalized + "?_foreign_keys=on"
}

func must(err error, action string) {
	if err != nil {
		fatalf("%s: %v", action, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func init() {
	flag.CommandLine.SetOutput(os.Stdout)
}
