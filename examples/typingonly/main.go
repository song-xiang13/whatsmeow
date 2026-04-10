package main

import (
	"context"
	"flag"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/song-xiang13/whatsmeow"
	"github.com/song-xiang13/whatsmeow/proto/waHistorySync"
	"github.com/song-xiang13/whatsmeow/store/sqlstore"
	"github.com/song-xiang13/whatsmeow/types"
	"github.com/song-xiang13/whatsmeow/types/events"
	waLog "github.com/song-xiang13/whatsmeow/util/log"
)

type sessionClient struct {
	DBPath string
	Client *whatsmeow.Client
	JID    types.JID
}

func main() {
	dbDirFlag := flag.String("db-dir", "", "Directory that contains multiple SQLite session databases")
	configFlag := flag.String("config", "", "Optional path to a whatsmeow client config JSON file")
	targetFlag := flag.String("target", "", "Account that receives typing status, supports phone number or full JID")
	excludeFlag := flag.String("exclude", "", "Comma-separated account list to exclude from helpers")
	socks5Flag := flag.String("socks5", "", "SOCKS5 proxy, supports host:port, user,pass,host:port, or socks5://user:pass@host:port")
	logLevelFlag := flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
	connectTimeoutFlag := flag.Duration("connect-timeout", 2*time.Minute, "Maximum time to wait for login")
	pulseFlag := flag.Duration("pulse", 8*time.Second, "Interval between typing pulses")
	typingDurationFlag := flag.Duration("typing-duration", 4*time.Second, "How long each typing pulse stays in composing state")
	flag.Parse()

	if *dbDirFlag == "" || *targetFlag == "" {
		flag.Usage()
		os.Exit(2)
	}

	targetJID, err := parseSingleTarget(*targetFlag)
	must(err, "parse target JID")

	excluded, err := parseTargetJIDs(*excludeFlag)
	must(err, "parse excluded helpers")
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, jid := range excluded {
		excludedSet[jid.ToNonAD().String()] = struct{}{}
	}

	proxyAddr := ""
	if *socks5Flag != "" {
		proxyAddr, err = normalizeSOCKS5Proxy(*socks5Flag)
		must(err, "parse socks5 proxy")
		fmt.Printf("using SOCKS5 proxy: %s\n", redactProxyURL(proxyAddr))
	}

	dbFiles, err := findDBFiles(*dbDirFlag)
	must(err, "scan db directory")
	if len(dbFiles) == 0 {
		fatalf("no .db files found in %s", *dbDirFlag)
	}

	helpers := make([]*sessionClient, 0, len(dbFiles))
	for _, dbPath := range dbFiles {
		client, err := openClient(dbPath, proxyAddr, *logLevelFlag, *configFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", dbPath, err)
			continue
		}
		if client.Store.ID == nil {
			fmt.Fprintf(os.Stderr, "skip %s: no stored login session\n", dbPath)
			continue
		}
		if err = ensureConnected(client, *connectTimeoutFlag, dbPath); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", dbPath, err)
			client.Disconnect()
			continue
		}
		setPresenceAvailable(client)

		jid := client.Store.GetJID()
		if _, blocked := excludedSet[jid.ToNonAD().String()]; blocked {
			fmt.Printf("exclude %s (%s)\n", filepath.Base(dbPath), jid.String())
			client.Disconnect()
			continue
		}

		helpers = append(helpers, &sessionClient{
			DBPath: dbPath,
			Client: client,
			JID:    jid,
		})
		fmt.Printf("connected helper %s as %s\n", filepath.Base(dbPath), jid.String())
	}

	if len(helpers) == 0 {
		fatalf("no helper accounts available")
	}
	defer disconnectSessions(helpers)

	fmt.Printf("typing target: %s\n", targetJID.String())
	fmt.Printf("helper count: %d\n", len(helpers))

	runTypingLoop(helpers, targetJID, *typingDurationFlag, *pulseFlag)
}

func runTypingLoop(helpers []*sessionClient, target types.JID, typingDuration, pulse time.Duration) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	fmt.Printf("typing-only loop running, press Ctrl+C to exit\n")
	for {
		fmt.Printf("typing pulse start to %s\n", target.String())
		setHelpersTyping(helpers, target, types.ChatPresenceComposing)

		timer := time.NewTimer(typingDuration)
		select {
		case <-stop:
			timer.Stop()
			setHelpersTyping(helpers, target, types.ChatPresencePaused)
			return
		case <-timer.C:
		}

		setHelpersTyping(helpers, target, types.ChatPresencePaused)
		fmt.Printf("typing pulse end to %s\n", target.String())

		wait := pulse - typingDuration
		if wait < 0 {
			wait = 0
		}
		timer = time.NewTimer(wait)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
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

func ensureConnected(client *whatsmeow.Client, timeout time.Duration, dbPath string) error {
	needsInitialSyncWait := client.Store.ID == nil
	monitor := newConnectMonitor(client)

	if client.Store.ID == nil {
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			return fmt.Errorf("get QR channel: %w", err)
		}
		if err = client.Connect(); err != nil {
			return fmt.Errorf("start QR login connection: %w", err)
		}
		go consumeQRChannel(qrChan, filepath.Join(filepath.Dir(dbPath), "login-qr.png"))
	} else {
		if err := client.Connect(); err != nil {
			return fmt.Errorf("connect with stored session: %w", err)
		}
	}

	if err := monitor.wait(timeout, client); err != nil {
		return err
	}
	if needsInitialSyncWait {
		waitTimeout := timeout
		if waitTimeout > 45*time.Second {
			waitTimeout = 45 * time.Second
		}
		if waitTimeout < 15*time.Second {
			waitTimeout = 15 * time.Second
		}
		if err := waitForInitialSync(client, waitTimeout); err != nil {
			return err
		}
	}
	return nil
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

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

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
		case <-ticker.C:
			fmt.Printf("waiting for WhatsApp connection... connected=%v logged_in=%v store_id=%v\n", client.IsConnected(), client.IsLoggedIn(), client.Store.ID != nil)
		case <-timer.C:
			return fmt.Errorf("client did not become ready within %s", timeout)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func waitForInitialSync(client *whatsmeow.Client, timeout time.Duration) error {
	done := make(chan string, 1)
	handlerID := client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.HistorySync:
			syncType := v.Data.GetSyncType()
			if syncType == waHistorySync.HistorySync_INITIAL_BOOTSTRAP ||
				syncType == waHistorySync.HistorySync_RECENT ||
				syncType == waHistorySync.HistorySync_PUSH_NAME {
				select {
				case done <- "history sync: " + syncType.String():
				default:
				}
			}
		case *events.AppStateSyncComplete:
			select {
			case done <- "app state sync: " + string(v.Name):
			default:
			}
		}
	})
	defer client.RemoveEventHandler(handlerID)

	fmt.Printf("waiting for initial sync after login...\n")
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case reason := <-done:
		fmt.Printf("initial sync completed: %s\n", reason)
		return nil
	case <-timer.C:
		return fmt.Errorf("initial sync did not complete within %s", timeout)
	}
}

func consumeQRChannel(qrChan <-chan whatsmeow.QRChannelItem, qrPath string) {
	for evt := range qrChan {
		if evt.Event == "code" {
			if err := writeQRPNG(qrPath, evt.Code); err != nil {
				fmt.Printf("qr code: %s\n", evt.Code)
				fmt.Printf("failed to write QR PNG: %v\n", err)
			} else {
				fmt.Printf("scan QR code at: %s\n", qrPath)
				fmt.Printf("if the image cannot be opened, use raw QR text: %s\n", evt.Code)
			}
			continue
		}
		fmt.Printf("login event: %s\n", evt.Event)
	}
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

func setHelpersTyping(helpers []*sessionClient, targetJID types.JID, state types.ChatPresence) {
	for _, helper := range helpers {
		if err := helper.Client.SendChatPresence(context.Background(), targetJID, state, types.ChatPresenceMediaText); err != nil {
			fmt.Fprintf(os.Stderr, "typing update failed from %s to %s: %v\n", helper.JID.String(), targetJID.String(), err)
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

func disconnectSessions(sessions []*sessionClient) {
	for _, session := range sessions {
		session.Client.Disconnect()
	}
}

func parseTargetJIDs(raw string) ([]types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
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

func writeQRPNG(path, content string) error {
	code, err := qr.Encode(content, qr.M, qr.Auto)
	if err != nil {
		return err
	}
	scaled, err := barcode.Scale(code, 256, 256)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return png.Encode(f, scaled)
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
