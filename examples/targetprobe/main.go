package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/song-xiang13/whatsmeow"
	"github.com/song-xiang13/whatsmeow/store/sqlstore"
	"github.com/song-xiang13/whatsmeow/types"
	"github.com/song-xiang13/whatsmeow/types/events"
	waLog "github.com/song-xiang13/whatsmeow/util/log"
)

const (
	newAccountQueryID      = "25808450525504568"
	usyncQueryID           = "29829202653362039"
	privacySettingsQueryID = "25637004609323493"
)

var privacyFeatures = []string{"LAST", "ONLINE", "PROFILE", "ABOUT", "READRECEIPTS", "GROUPADD", "CALLADD", "STICKERS", "MESSAGES", "DEFENSE"}

type queryInput struct {
	JID              string         `json:"jid"`
	IntegritySignals map[string]any `json:"integrity_signals,omitempty"`
	PrivacyFeatures  []string       `json:"privacy_features,omitempty"`
}

type newAccountResponse struct {
	Users []struct {
		ID                   string `json:"id"`
		IntegritySignalsInfo *struct {
			TypeName     string `json:"__typename"`
			IsNewAccount *bool  `json:"is_new_account"`
		} `json:"integrity_signals_info"`
	} `json:"xwa2_fetch_wa_users"`
}

type usyncResponse struct {
	Users []struct {
		JID          string `json:"jid"`
		CountryCode  any    `json:"country_code"`
		UsernameInfo *struct {
			TypeName  string `json:"__typename"`
			Username  string `json:"username"`
			State     string `json:"state"`
			Timestamp any    `json:"timestamp"`
			Pin       string `json:"pin"`
			Status    string `json:"status"`
		} `json:"username_info"`
		AboutStatusInfo *struct {
			TypeName  string `json:"__typename"`
			Text      string `json:"text"`
			Timestamp any    `json:"timestamp"`
			Status    string `json:"status"`
		} `json:"about_status_info"`
	} `json:"xwa2_fetch_wa_users"`
}

type privacySettingsResponse struct {
	Users []struct {
		ID              string `json:"id"`
		PrivacySettings *struct {
			Settings []struct {
				Feature string `json:"feature"`
				Setting string `json:"setting"`
			} `json:"settings"`
		} `json:"privacy_settings"`
	} `json:"xwa2_fetch_wa_users"`
}

type probeRow struct {
	InputNumber       string            `json:"input_number"`
	JID               string            `json:"jid"`
	IsNewAccount      *bool             `json:"is_new_account,omitempty"`
	IntegrityTypeName string            `json:"integrity_typename,omitempty"`
	CountryCode       string            `json:"country_code,omitempty"`
	Username          string            `json:"username,omitempty"`
	UsernameState     string            `json:"username_state,omitempty"`
	UsernameTimestamp string            `json:"username_timestamp,omitempty"`
	UsernamePIN       string            `json:"username_pin,omitempty"`
	AboutText         string            `json:"about_text,omitempty"`
	AboutState        string            `json:"about_state,omitempty"`
	AboutTimestamp    string            `json:"about_timestamp,omitempty"`
	PrivacySettings   map[string]string `json:"privacy_settings,omitempty"`
	SignalScore       int               `json:"signal_score"`
	SignalLabel       string            `json:"signal_label"`
	SignalReasons     []string          `json:"signal_reasons,omitempty"`
	Errors            []string          `json:"errors,omitempty"`
}

func main() {
	dbFlag := flag.String("db", "", "Path to the SQLite session database")
	idsFileFlag := flag.String("ids-file", "", "Path to a text file that contains phone numbers")
	configFlag := flag.String("config", "", "Optional client config JSON file")
	socks5Flag := flag.String("socks5", "", "Optional SOCKS5 proxy URL")
	outFlag := flag.String("out", "", "Optional local JSON output file")
	timeoutFlag := flag.Duration("connect-timeout", 2*time.Minute, "Maximum time to wait for login")
	logLevelFlag := flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
	flag.Parse()

	if *dbFlag == "" || *idsFileFlag == "" {
		flag.Usage()
		os.Exit(2)
	}

	ids, err := extractNumbersFromFile(*idsFileFlag)
	must(err, "extract numbers")
	if len(ids) == 0 {
		fatalf("no IDs found in %s", *idsFileFlag)
	}

	client, err := openClient(*dbFlag, *socks5Flag, *logLevelFlag, *configFlag)
	must(err, "open client")
	defer client.Disconnect()
	must(ensureConnected(client, *timeoutFlag), "connect WhatsApp client")

	rows := probeTargets(client, ids)
	printSummary(rows)
	if strings.TrimSpace(*outFlag) != "" {
		must(writeJSON(*outFlag, rows), "write output file")
		fmt.Printf("saved output to %s\n", *outFlag)
	}
}

func probeTargets(client *whatsmeow.Client, ids []string) []probeRow {
	ctx := context.Background()
	internal := client.DangerousInternals()
	rows := make([]probeRow, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, probeRow{
			InputNumber: id,
			JID:         types.NewJID(id, types.DefaultUserServer).String(),
		})
	}

	newAccountRows := batchNewAccount(ctx, internal, rows)
	usyncRows := batchUSync(ctx, internal, rows)

	for i := range rows {
		if newRow, ok := newAccountRows[rows[i].JID]; ok {
			rows[i].IsNewAccount = newRow.IsNewAccount
			rows[i].IntegrityTypeName = newRow.TypeName
		}
		if usyncRow, ok := usyncRows[rows[i].JID]; ok {
			rows[i].CountryCode = usyncRow.CountryCode
			rows[i].Username = usyncRow.Username
			rows[i].UsernameState = usyncRow.UsernameState
			rows[i].UsernameTimestamp = usyncRow.UsernameTimestamp
			rows[i].UsernamePIN = usyncRow.UsernamePIN
			rows[i].AboutText = usyncRow.AboutText
			rows[i].AboutState = usyncRow.AboutState
			rows[i].AboutTimestamp = usyncRow.AboutTimestamp
		}
		if settings, err := fetchPrivacySettings(ctx, internal, rows[i].JID); err != nil {
			rows[i].Errors = append(rows[i].Errors, "privacy_settings: "+err.Error())
		} else {
			rows[i].PrivacySettings = settings
		}
		rows[i].SignalScore, rows[i].SignalLabel, rows[i].SignalReasons = scoreRow(rows[i])
	}

	return rows
}

type newAccountRow struct {
	IsNewAccount *bool
	TypeName     string
}

func batchNewAccount(ctx context.Context, internal *whatsmeow.DangerousInternalClient, rows []probeRow) map[string]newAccountRow {
	inputs := make([]queryInput, 0, len(rows))
	for _, row := range rows {
		inputs = append(inputs, queryInput{
			JID: row.JID,
			IntegritySignals: map[string]any{
				"use_case": "START_CHAT_CONTEXT",
			},
		})
	}
	raw, err := internal.SendMexIQ(ctx, newAccountQueryID, map[string]any{
		"input": map[string]any{
			"query_input": inputs,
			"telemetry": map[string]any{
				"context": "INTERACTIVE",
			},
		},
	})
	if err != nil {
		return map[string]newAccountRow{}
	}

	var resp newAccountResponse
	if json.Unmarshal(raw, &resp) != nil {
		return map[string]newAccountRow{}
	}

	out := make(map[string]newAccountRow, len(rows))
	for idx, user := range resp.Users {
		jid := ""
		if idx < len(rows) {
			jid = rows[idx].JID
		}
		if user.ID != "" {
			jid = user.ID
		}
		row := newAccountRow{}
		if user.IntegritySignalsInfo != nil {
			row.IsNewAccount = user.IntegritySignalsInfo.IsNewAccount
			row.TypeName = user.IntegritySignalsInfo.TypeName
		}
		if jid != "" {
			out[jid] = row
		}
	}
	return out
}

type usyncRow struct {
	CountryCode       string
	Username          string
	UsernameState     string
	UsernameTimestamp string
	UsernamePIN       string
	AboutText         string
	AboutState        string
	AboutTimestamp    string
}

func batchUSync(ctx context.Context, internal *whatsmeow.DangerousInternalClient, rows []probeRow) map[string]usyncRow {
	users := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		users = append(users, map[string]any{"jid": row.JID})
	}
	raw, err := internal.SendMexIQ(ctx, usyncQueryID, map[string]any{
		"input": map[string]any{
			"query_input": users,
			"telemetry": map[string]any{
				"context": "INTERACTIVE",
			},
		},
		"include_username":     true,
		"include_about_status": true,
		"include_country_code": true,
	})
	if err != nil {
		return map[string]usyncRow{}
	}
	var resp usyncResponse
	if json.Unmarshal(raw, &resp) != nil {
		return map[string]usyncRow{}
	}
	out := make(map[string]usyncRow, len(resp.Users))
	for _, user := range resp.Users {
		row := usyncRow{
			CountryCode: stringifyAny(user.CountryCode),
		}
		if user.UsernameInfo != nil {
			row.Username = user.UsernameInfo.Username
			row.UsernameState = firstNonEmpty(user.UsernameInfo.State, user.UsernameInfo.Status)
			row.UsernameTimestamp = stringifyAny(user.UsernameInfo.Timestamp)
			row.UsernamePIN = user.UsernameInfo.Pin
		}
		if user.AboutStatusInfo != nil {
			row.AboutText = user.AboutStatusInfo.Text
			row.AboutState = user.AboutStatusInfo.Status
			row.AboutTimestamp = stringifyAny(user.AboutStatusInfo.Timestamp)
		}
		out[user.JID] = row
	}
	return out
}

func fetchPrivacySettings(ctx context.Context, internal *whatsmeow.DangerousInternalClient, jid string) (map[string]string, error) {
	raw, err := internal.SendMexIQ(ctx, privacySettingsQueryID, map[string]any{
		"input": map[string]any{
			"query_input": []queryInput{{
				JID:             jid,
				PrivacyFeatures: privacyFeatures,
			}},
		},
	})
	if err != nil {
		return nil, err
	}
	var resp privacySettingsResponse
	if err = json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Users) == 0 || resp.Users[0].PrivacySettings == nil {
		return nil, fmt.Errorf("privacy_settings missing")
	}
	out := make(map[string]string, len(resp.Users[0].PrivacySettings.Settings))
	for _, item := range resp.Users[0].PrivacySettings.Settings {
		if item.Feature == "" {
			continue
		}
		out[item.Feature] = item.Setting
	}
	return out, nil
}

func scoreRow(row probeRow) (int, string, []string) {
	score := 0
	var reasons []string

	switch {
	case row.IsNewAccount == nil:
		reasons = append(reasons, "missing_new_account_signal")
	case *row.IsNewAccount:
		score -= 45
		reasons = append(reasons, "is_new_account=true")
	default:
		score += 45
		reasons = append(reasons, "is_new_account=false")
	}

	if row.Username != "" {
		score += 15
		reasons = append(reasons, "has_username")
	}
	if row.AboutText != "" {
		score += 10
		reasons = append(reasons, "has_about_text")
	}
	if len(row.PrivacySettings) >= 6 {
		score += 10
		reasons = append(reasons, "privacy_settings_populated")
	}
	if row.AboutState == "NOT_ALLOWED" {
		score += 4
		reasons = append(reasons, "about_privacy_enforced")
	}
	if row.CountryCode != "" {
		score += 2
		reasons = append(reasons, "country_code_present")
	}

	label := "mixed"
	switch {
	case score >= 55:
		label = "likely_older_signal"
	case score <= 0:
		label = "likely_new_or_sparse_signal"
	}
	return score, label, reasons
}

func printSummary(rows []probeRow) {
	fmt.Printf("rows=%d\n", len(rows))
	for _, row := range rows {
		newAccount := "null"
		if row.IsNewAccount != nil {
			newAccount = fmt.Sprintf("%v", *row.IsNewAccount)
		}
		username := row.Username
		if username == "" {
			username = "-"
		}
		aboutState := row.AboutState
		if aboutState == "" {
			aboutState = "-"
		}
		fmt.Printf("%s\tnew=%s\tscore=%d\tlabel=%s\tuser=%s\tabout_state=%s\tprivacy=%d\n",
			row.InputNumber, newAccount, row.SignalScore, row.SignalLabel, username, aboutState, len(row.PrivacySettings))
	}
}

func writeJSON(path string, rows []probeRow) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func extractNumbersFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	lineDigitsRe := regexp.MustCompile(`(?m)^\s*([0-9]{6,})\s*$`)

	seen := make(map[string]struct{})
	var ids []string
	for _, match := range lineDigitsRe.FindAllStringSubmatch(content, -1) {
		id := strings.TrimSpace(match[1])
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func stringifyAny(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

func firstNonEmpty(values ...string) string {
	for _, item := range values {
		if item != "" {
			return item
		}
	}
	return ""
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
