package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/song-xiang13/whatsmeow"
	"github.com/song-xiang13/whatsmeow/proto/waE2E"
	"github.com/song-xiang13/whatsmeow/store/sqlstore"
	"github.com/song-xiang13/whatsmeow/types"
	"github.com/song-xiang13/whatsmeow/types/events"
	waLog "github.com/song-xiang13/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type accountGroup string

const (
	groupWithoutConfig accountGroup = "without_config"
	groupWithConfig    accountGroup = "with_config"
)

type messageRecord struct {
	AccountDB     string `json:"account_db"`
	AccountJID    string `json:"account_jid"`
	Group         string `json:"group"`
	UseConfig     bool   `json:"use_config"`
	Target        string `json:"target"`
	AttemptTime   string `json:"attempt_time"`
	Success       bool   `json:"success"`
	MessageID     string `json:"message_id,omitempty"`
	ServerTime    string `json:"server_time,omitempty"`
	Error         string `json:"error,omitempty"`
	StopTriggered bool   `json:"stop_triggered"`
}

type eventRecord struct {
	AccountDB  string `json:"account_db"`
	AccountJID string `json:"account_jid"`
	Group      string `json:"group"`
	Time       string `json:"time"`
	Type       string `json:"type"`
	Reason     string `json:"reason,omitempty"`
}

type accountSummary struct {
	DBPath            string `json:"db_path"`
	AccountJID        string `json:"account_jid"`
	Group             string `json:"group"`
	UseConfig         bool   `json:"use_config"`
	Proxy             string `json:"proxy,omitempty"`
	ConfigPath        string `json:"config_path,omitempty"`
	StartTime         string `json:"start_time,omitempty"`
	StopTime          string `json:"stop_time,omitempty"`
	StopReason        string `json:"stop_reason,omitempty"`
	Connected         bool   `json:"connected"`
	Assigned          int    `json:"assigned"`
	Attempted         int    `json:"attempted"`
	Success           int    `json:"success"`
	Failed            int    `json:"failed"`
	ConsecutiveFailed int    `json:"consecutive_failed"`
	FirstErrorTime    string `json:"first_error_time,omitempty"`
}

type report struct {
	StartedAt      string           `json:"started_at"`
	FinishedAt     string           `json:"finished_at"`
	DBDir          string           `json:"db_dir"`
	IDsFile        string           `json:"ids_file"`
	Text           string           `json:"text"`
	ConfigPath     string           `json:"config_path"`
	Proxy          string           `json:"proxy,omitempty"`
	Seed           int64            `json:"seed"`
	Interval       string           `json:"interval"`
	MaxPerAccount  int              `json:"max_per_account"`
	StopAfterFails int              `json:"stop_after_fails"`
	Accounts       []accountSummary `json:"accounts"`
	Messages       []messageRecord  `json:"messages"`
	Events         []eventRecord    `json:"events"`
}

type stopState struct {
	reason string
	when   time.Time
}

type testAccount struct {
	dbPath       string
	group        accountGroup
	useConfig    bool
	client       *whatsmeow.Client
	jid          types.JID
	assignedTargets []types.JID
	summary      accountSummary
	stop         stopState
	messageLog   *[]messageRecord
	eventLog     *[]eventRecord
	logMu        *sync.Mutex
	stopMu       sync.Mutex
	stopFailOnce bool
}

func main() {
	dbDirFlag := flag.String("db-dir", "", "Directory that contains SQLite session databases")
	idsFileFlag := flag.String("ids-file", "", "Path to a text file that contains target phone numbers")
	textFlag := flag.String("text", "", "Text message to send")
	configFlag := flag.String("config", "", "Path to a whatsmeow client config JSON file used by the test group")
	socks5Flag := flag.String("socks5", "", "SOCKS5 proxy, supports host:port, user,pass,host:port, or socks5://user:pass@host:port")
	logLevelFlag := flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
	connectTimeoutFlag := flag.Duration("connect-timeout", 2*time.Minute, "Maximum time to wait for login")
	intervalFlag := flag.Duration("interval", 3*time.Second, "Delay between recipients")
	perAccountFlag := flag.Int("per-account", 60, "How many IDs to randomly assign to each account")
	stopAfterFailuresFlag := flag.Int("stop-after-failures", 5, "Stop an account after this many consecutive send failures")
	seedFlag := flag.Int64("seed", time.Now().UnixNano(), "Random seed used to shuffle accounts and IDs")
	outDirFlag := flag.String("out-dir", "", "Output directory for JSON, CSV and Markdown reports")
	flag.Parse()

	if strings.TrimSpace(*dbDirFlag) == "" || strings.TrimSpace(*idsFileFlag) == "" || strings.TrimSpace(*textFlag) == "" {
		flag.Usage()
		os.Exit(2)
	}
	if strings.TrimSpace(*configFlag) == "" {
		fatalf("-config is required for the with_config group")
	}

	proxyAddr := ""
	var err error
	if *socks5Flag != "" {
		proxyAddr, err = normalizeSOCKS5Proxy(*socks5Flag)
		must(err, "parse socks5 proxy")
		fmt.Printf("using SOCKS5 proxy: %s\n", redactProxyURL(proxyAddr))
	}

	outDir := strings.TrimSpace(*outDirFlag)
	if outDir == "" {
		outDir = filepath.Join(filepath.Dir(*idsFileFlag), "sendtextab-"+time.Now().Format("20060102-150405"))
	}
	must(os.MkdirAll(outDir, 0o755), "create output directory")

	startedAt := time.Now()
	ids, err := loadIDs(*idsFileFlag)
	must(err, "load ids file")
	if len(ids) == 0 {
		fatalf("no valid IDs found in %s", *idsFileFlag)
	}

	dbFiles, err := findDBFiles(*dbDirFlag)
	must(err, "scan db directory")
	if len(dbFiles) < 2 {
		fatalf("need at least 2 .db files in %s", *dbDirFlag)
	}

	rng := rand.New(rand.NewSource(*seedFlag))
	rng.Shuffle(len(dbFiles), func(i, j int) {
		dbFiles[i], dbFiles[j] = dbFiles[j], dbFiles[i]
	})

	withCount, withoutCount := splitCounts(len(dbFiles))
	messageLog := make([]messageRecord, 0, len(ids))
	eventLog := make([]eventRecord, 0, len(dbFiles)*4)
	logMu := &sync.Mutex{}
	accounts := make([]*testAccount, len(dbFiles))

	var connectWG sync.WaitGroup
	for idx, dbPath := range dbFiles {
		group := groupWithoutConfig
		useConfig := false
		accountConfig := ""
		if idx >= withoutCount && idx < withoutCount+withCount {
			group = groupWithConfig
			useConfig = true
			accountConfig = *configFlag
		}

		account := &testAccount{
			dbPath:     dbPath,
			group:      group,
			useConfig:  useConfig,
			messageLog: &messageLog,
			eventLog:   &eventLog,
			logMu:      logMu,
			summary: accountSummary{
				DBPath:     dbPath,
				Group:      string(group),
				UseConfig:  useConfig,
				Proxy:      redactProxyURL(proxyAddr),
				ConfigPath: accountConfig,
			},
		}
		accounts[idx] = account

		connectWG.Add(1)
		go func(account *testAccount, dbPath, accountConfig string, group accountGroup) {
			defer connectWG.Done()

			client, err := openClient(dbPath, proxyAddr, *logLevelFlag, accountConfig)
			if err != nil {
				account.stopNow("open_client_failed: " + err.Error())
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", dbPath, err)
				return
			}
			account.client = client
			account.installStopMonitor()

			if client.Store.ID == nil {
				account.stopNow("no_stored_login_session")
				fmt.Fprintf(os.Stderr, "skip %s: no stored login session\n", dbPath)
				client.Disconnect()
				return
			}
			if err = ensureConnected(client, *connectTimeoutFlag, dbPath, false); err != nil {
				account.stopNow("connect_failed: " + err.Error())
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", dbPath, err)
				client.Disconnect()
				return
			}

			account.summary.Connected = true
			account.summary.StartTime = time.Now().Format(time.RFC3339)
			account.jid = client.Store.GetJID()
			account.summary.AccountJID = account.jid.String()
			setPresenceAvailable(client)
			fmt.Printf("connected %s as %s (%s)\n", filepath.Base(dbPath), account.jid.String(), group)
		}(account, dbPath, accountConfig, group)
	}
	connectWG.Wait()

	connectedAccounts := make([]*testAccount, 0, len(accounts))
	for _, account := range accounts {
		if account != nil && account.summary.Connected && account.client != nil {
			connectedAccounts = append(connectedAccounts, account)
		}
	}
	accounts = connectedAccounts

	if len(accounts) < 2 {
		fatalf("need at least 2 connected accounts to run the test")
	}
	defer disconnectAccounts(accounts)

	assignTargets(accounts, ids, *perAccountFlag, *seedFlag)

	var wg sync.WaitGroup
	for _, account := range accounts {
		if account.shouldStop() {
			continue
		}
		wg.Add(1)
		go func(account *testAccount) {
			defer wg.Done()
			runAccountSendLoop(account, *textFlag, *intervalFlag, *stopAfterFailuresFlag)
		}(account)
	}
	wg.Wait()

	rep := report{
		StartedAt:      startedAt.Format(time.RFC3339),
		FinishedAt:     time.Now().Format(time.RFC3339),
		DBDir:          *dbDirFlag,
		IDsFile:        *idsFileFlag,
		Text:           *textFlag,
		ConfigPath:     *configFlag,
		Proxy:          redactProxyURL(proxyAddr),
		Seed:           *seedFlag,
		Interval:       intervalFlag.String(),
		MaxPerAccount:  *perAccountFlag,
		StopAfterFails: *stopAfterFailuresFlag,
		Accounts:       collectSummaries(accounts),
		Messages:       messageLog,
		Events:         eventLog,
	}

	must(writeJSON(filepath.Join(outDir, "results.json"), rep), "write results.json")
	must(writeMessagesCSV(filepath.Join(outDir, "messages.csv"), messageLog), "write messages.csv")
	must(writeMarkdownReport(filepath.Join(outDir, "report.md"), rep), "write report.md")

	fmt.Printf("report written to %s\n", outDir)
}

func splitCounts(total int) (withoutCount, withCount int) {
	withoutCount = total / 2
	withCount = total - withoutCount
	if withoutCount == 0 {
		withoutCount = 1
	}
	if withCount == 0 {
		withCount = 1
	}
	return withoutCount, withCount
}

func assignTargets(accounts []*testAccount, ids []types.JID, perAccount int, seed int64) {
	if len(accounts) == 0 {
		return
	}
	if perAccount <= 0 {
		perAccount = 60
	}
	pool := append([]types.JID(nil), ids...)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(pool), func(i, j int) {
		pool[i], pool[j] = pool[j], pool[i]
	})

	cursor := 0
	for _, account := range accounts {
		if cursor >= len(pool) {
			account.summary.Assigned = 0
			account.assignedTargets = nil
			continue
		}
		take := perAccount
		remaining := len(pool) - cursor
		if take > remaining {
			take = remaining
		}
		account.summary.Assigned = take
		account.assignedTargets = append([]types.JID(nil), pool[cursor:cursor+take]...)
		cursor += take
	}
}

func runAccountSendLoop(account *testAccount, text string, interval time.Duration, stopAfterFailures int) {
	targets := account.assignedTargets
	if len(targets) == 0 {
		account.stopNow("no_assigned_targets")
		return
	}

	prewarmTargets(account.client, targets)
	for i, target := range targets {
		if account.shouldStop() {
			break
		}
		if i > 0 && interval > 0 {
			time.Sleep(interval)
		}

		account.summary.Attempted++
		resp, err := account.client.SendMessage(context.Background(), target, &waE2E.Message{
			Conversation: proto.String(text),
		})
		record := messageRecord{
			AccountDB:   account.dbPath,
			AccountJID:  account.jid.String(),
			Group:       string(account.group),
			UseConfig:   account.useConfig,
			Target:      target.String(),
			AttemptTime: time.Now().Format(time.RFC3339),
		}
		if err != nil {
			account.summary.Failed++
			account.summary.ConsecutiveFailed++
			if account.summary.FirstErrorTime == "" {
				account.summary.FirstErrorTime = record.AttemptTime
			}
			record.Error = err.Error()
			if stopAfterFailures > 0 && account.summary.ConsecutiveFailed >= stopAfterFailures {
				account.stopNow(fmt.Sprintf("consecutive_send_failures:%d", account.summary.ConsecutiveFailed))
				record.StopTriggered = true
			}
		} else {
			account.summary.Success++
			account.summary.ConsecutiveFailed = 0
			record.Success = true
			record.MessageID = resp.ID
			record.ServerTime = resp.Timestamp.Format(time.RFC3339)
		}
		account.appendMessage(record)
	}
	if !account.shouldStop() {
		account.stopNow("assigned_targets_exhausted")
	}
}

func (ta *testAccount) installStopMonitor() {
	ta.client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.LoggedOut:
			ta.logEvent("logged_out", v.Reason.String())
			ta.stopNow("logged_out:" + v.Reason.String())
		case *events.TemporaryBan:
			ta.logEvent("temporary_ban", v.String())
			ta.stopNow("temporary_ban:" + v.String())
		case *events.StreamReplaced:
			ta.logEvent("stream_replaced", "session was taken over by another client")
			ta.stopNow("stream_replaced")
		case *events.ClientOutdated:
			ta.logEvent("client_outdated", "client version rejected by WhatsApp")
			ta.stopNow("client_outdated")
		case *events.ConnectFailure:
			reason := v.Reason.String()
			ta.logEvent("connect_failure", reason+" "+v.Message)
			ta.stopNow("connect_failure:" + reason)
		case *events.CATRefreshError:
			ta.logEvent("cat_refresh_error", v.Error.Error())
			ta.stopNow("cat_refresh_error")
		}
	})
}

func (ta *testAccount) logEvent(eventType, reason string) {
	ta.logMu.Lock()
	*ta.eventLog = append(*ta.eventLog, eventRecord{
		AccountDB:  ta.dbPath,
		AccountJID: ta.jid.String(),
		Group:      string(ta.group),
		Time:       time.Now().Format(time.RFC3339),
		Type:       eventType,
		Reason:     reason,
	})
	ta.logMu.Unlock()
}

func (ta *testAccount) appendMessage(record messageRecord) {
	ta.logMu.Lock()
	*ta.messageLog = append(*ta.messageLog, record)
	ta.logMu.Unlock()
}

func (ta *testAccount) stopNow(reason string) {
	ta.stopMu.Lock()
	defer ta.stopMu.Unlock()
	if ta.stop.reason != "" {
		return
	}
	ta.stop = stopState{reason: reason, when: time.Now()}
	ta.summary.StopReason = reason
	ta.summary.StopTime = ta.stop.when.Format(time.RFC3339)
}

func (ta *testAccount) shouldStop() bool {
	ta.stopMu.Lock()
	defer ta.stopMu.Unlock()
	return ta.stop.reason != ""
}

func collectSummaries(accounts []*testAccount) []accountSummary {
	out := make([]accountSummary, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, account.summary)
	}
	return out
}

func loadIDs(path string) ([]types.JID, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fields := strings.FieldsFunc(string(data), func(r rune) bool {
		switch r {
		case '\n', '\r', '\t', ',', ' ', ';':
			return true
		default:
			return false
		}
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]types.JID, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		jid, err := parseSingleTarget(field)
		if err != nil {
			return nil, err
		}
		key := jid.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, jid)
	}
	return out, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeMessagesCSV(path string, records []messageRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err = w.Write([]string{"account_db", "account_jid", "group", "use_config", "target", "attempt_time", "success", "message_id", "server_time", "error", "stop_triggered"}); err != nil {
		return err
	}
	for _, record := range records {
		row := []string{
			record.AccountDB,
			record.AccountJID,
			record.Group,
			fmt.Sprintf("%t", record.UseConfig),
			record.Target,
			record.AttemptTime,
			fmt.Sprintf("%t", record.Success),
			record.MessageID,
			record.ServerTime,
			record.Error,
			fmt.Sprintf("%t", record.StopTriggered),
		}
		if err = w.Write(row); err != nil {
			return err
		}
	}
	return w.Error()
}

func writeMarkdownReport(path string, rep report) error {
	var b strings.Builder
	b.WriteString("# SendText A/B Report\n\n")
	b.WriteString(fmt.Sprintf("- Started: %s\n", rep.StartedAt))
	b.WriteString(fmt.Sprintf("- Finished: %s\n", rep.FinishedAt))
	b.WriteString(fmt.Sprintf("- DB Dir: `%s`\n", rep.DBDir))
	b.WriteString(fmt.Sprintf("- IDs File: `%s`\n", rep.IDsFile))
	b.WriteString(fmt.Sprintf("- Seed: `%d`\n", rep.Seed))
	b.WriteString(fmt.Sprintf("- Interval: `%s`\n", rep.Interval))
	b.WriteString(fmt.Sprintf("- Stop After Consecutive Failures: `%d`\n\n", rep.StopAfterFails))
	b.WriteString("## Accounts\n\n")
	b.WriteString("| Group | JID | DB | Assigned | Attempted | Success | Failed | Stop Reason |\n")
	b.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: | --- |\n")
	for _, account := range rep.Accounts {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %d | %d | %d | %s |\n",
			account.Group,
			account.AccountJID,
			filepath.Base(account.DBPath),
			account.Assigned,
			account.Attempted,
			account.Success,
			account.Failed,
			account.StopReason,
		))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
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
	if proxyAddr != "" {
		if err = client.SetProxyAddress(proxyAddr); err != nil {
			return nil, fmt.Errorf("configure socks5 proxy: %w", err)
		}
	}
	return client, nil
}

func setPresenceAvailable(client *whatsmeow.Client) {
	if strings.TrimSpace(client.Store.PushName) == "" {
		fmt.Fprintf(os.Stderr, "warn: PushName is empty, skip SendPresence(available)\n")
		return
	}
	if err := client.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to set presence available: %v\n", err)
	}
}

func prewarmTargets(client *whatsmeow.Client, targets []types.JID) {
	uniqueTargets := uniqueJIDs(targets)
	if len(uniqueTargets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, _ = client.GetUserDevices(ctx, uniqueTargets)
	_, _ = client.GetUserInfo(ctx, uniqueTargets)
}

func ensureConnected(client *whatsmeow.Client, timeout time.Duration, dbPath string, allowQR bool) error {
	_ = dbPath
	_ = allowQR
	monitor := newConnectMonitor(client)
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

func findDBFiles(dbDir string) ([]string, error) {
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if filepath.Ext(name) != ".db" {
			continue
		}
		files = append(files, filepath.Join(dbDir, entry.Name()))
	}
	return files, nil
}

func disconnectAccounts(accounts []*testAccount) {
	for _, account := range accounts {
		if account.client != nil {
			account.client.Disconnect()
		}
	}
}

func uniqueJIDs(input []types.JID) []types.JID {
	seen := make(map[string]struct{}, len(input))
	out := make([]types.JID, 0, len(input))
	for _, jid := range input {
		key := jid.ToNonAD().String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, jid)
	}
	return out
}

func parseTargetJIDs(raw string) ([]types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty target")
	}
	parts := strings.Split(raw, ",")
	targets := make([]types.JID, 0, len(parts))
	for _, part := range parts {
		target, err := parseSingleTarget(part)
		if err != nil {
			return nil, err
		}
		if !target.IsEmpty() {
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("empty target list")
	}
	return targets, nil
}

func parseSingleTarget(raw string) (types.JID, error) {
	part := strings.TrimSpace(raw)
	if part == "" {
		return types.JID{}, nil
	}
	if strings.Contains(part, "@") {
		target, err := types.ParseJID(part)
		if err != nil {
			return types.JID{}, fmt.Errorf("parse target %q: %w", part, err)
		}
		return target, nil
	}
	return types.NewJID(part, types.DefaultUserServer), nil
}

func normalizeSOCKS5Proxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty socks5 proxy")
	}
	if strings.HasPrefix(strings.ToLower(raw), "socks5://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		if parsed.Host == "" {
			return "", fmt.Errorf("missing socks5 host:port")
		}
		return parsed.String(), nil
	}

	parts := strings.Split(raw, ",")
	switch len(parts) {
	case 1:
		host := strings.TrimSpace(parts[0])
		if host == "" {
			return "", fmt.Errorf("missing socks5 host:port")
		}
		return "socks5://" + host, nil
	case 3:
		user := url.QueryEscape(strings.TrimSpace(parts[0]))
		pass := url.QueryEscape(strings.TrimSpace(parts[1]))
		host := strings.TrimSpace(parts[2])
		if host == "" {
			return "", fmt.Errorf("missing socks5 host:port")
		}
		return fmt.Sprintf("socks5://%s:%s@%s", user, pass, host), nil
	default:
		return "", fmt.Errorf("invalid socks5 format, use host:port, user,pass,host:port, or socks5://user:pass@host:port")
	}
}

func redactProxyURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	username := parsed.User.Username()
	if username != "" {
		parsed.User = url.UserPassword(username, "******")
	} else {
		parsed.User = nil
	}
	return parsed.String()
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
