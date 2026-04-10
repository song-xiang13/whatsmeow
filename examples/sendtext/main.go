package main

import (
	"bufio"
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
	"github.com/song-xiang13/whatsmeow/proto/waE2E"
	"github.com/song-xiang13/whatsmeow/proto/waHistorySync"
	"github.com/song-xiang13/whatsmeow/store/sqlstore"
	"github.com/song-xiang13/whatsmeow/types"
	"github.com/song-xiang13/whatsmeow/types/events"
	waLog "github.com/song-xiang13/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type sessionClient struct {
	DBPath string
	Client *whatsmeow.Client
	JID    types.JID
}

func main() {
	toFlag := flag.String("to", "", "Target phone number(s) or JID(s), comma-separated")
	textFlag := flag.String("text", "", "Text message to send")
	dbFlag := flag.String("db", "", "Path to a single SQLite session database")
	dbDirFlag := flag.String("db-dir", "", "Directory that contains multiple SQLite session databases")
	configFlag := flag.String("config", "", "Optional path to a whatsmeow client config JSON file")
	senderFlag := flag.String("sender", "", "Sender account when using -db-dir, supports phone number or full JID")
	socks5Flag := flag.String("socks5", "", "SOCKS5 proxy, supports host:port, user,pass,host:port, or socks5://user:pass@host:port")
	logLevelFlag := flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
	connectTimeoutFlag := flag.Duration("connect-timeout", 2*time.Minute, "Maximum time to wait for login")
	intervalFlag := flag.Duration("interval", 3*time.Second, "Delay between recipients")
	typingDurationFlag := flag.Duration("typing-duration", 3*time.Second, "How long helper accounts stay in composing state before each send")
	holdFlag := flag.Bool("hold", false, "Keep all connected clients running until Ctrl+C")
	flag.Parse()

	if *toFlag == "" || *textFlag == "" {
		flag.Usage()
		os.Exit(2)
	}
	if (*dbFlag == "" && *dbDirFlag == "") || (*dbFlag != "" && *dbDirFlag != "") {
		fatalf("exactly one of -db or -db-dir must be provided")
	}

	targets, err := parseTargetJIDs(*toFlag)
	must(err, "parse target JIDs")

	proxyAddr := ""
	if *socks5Flag != "" {
		proxyAddr, err = normalizeSOCKS5Proxy(*socks5Flag)
		must(err, "parse socks5 proxy")
		fmt.Printf("using SOCKS5 proxy: %s\n", redactProxyURL(proxyAddr))
	}

	if *dbDirFlag != "" {
		runMultiDB(targets, *textFlag, *dbDirFlag, *senderFlag, proxyAddr, *logLevelFlag, *configFlag, *connectTimeoutFlag, *typingDurationFlag, *intervalFlag)
		return
	}

	runSingleDB(targets, *textFlag, *dbFlag, proxyAddr, *logLevelFlag, *configFlag, *connectTimeoutFlag, *intervalFlag, *holdFlag)
}

func runSingleDB(targets []types.JID, text, dbPath, proxyAddr, logLevel, configPath string, connectTimeout, interval time.Duration, hold bool) {
	client, err := openClient(dbPath, proxyAddr, logLevel, configPath)
	must(err, "open session client")
	defer client.Disconnect()

	must(ensureConnected(client, connectTimeout, dbPath, true), "connect WhatsApp client")
	setPresenceAvailable(client)
	prewarmTargets(client, targets)

	runSingleSendLoop(client, targets, text, interval)
	if hold {
		waitForInterrupt("single client connected")
	}
}

func runMultiDB(targets []types.JID, text, dbDir, senderRaw, proxyAddr, logLevel, configPath string, connectTimeout, typingDuration, interval time.Duration) {
	if senderRaw == "" {
		fatalf("-sender is required when using -db-dir")
	}

	dbFiles, err := findDBFiles(dbDir)
	must(err, "scan db directory")
	if len(dbFiles) == 0 {
		fatalf("no .db files found in %s", dbDir)
	}

	sessions := make([]*sessionClient, 0, len(dbFiles))
	for _, dbPath := range dbFiles {
		client, err := openClient(dbPath, proxyAddr, logLevel, configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", dbPath, err)
			continue
		}
		if client.Store.ID == nil {
			fmt.Fprintf(os.Stderr, "skip %s: no stored login session\n", dbPath)
			continue
		}
		if err = ensureConnected(client, connectTimeout, dbPath, false); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", dbPath, err)
			client.Disconnect()
			continue
		}
		setPresenceAvailable(client)
		jid := client.Store.GetJID()
		sessions = append(sessions, &sessionClient{
			DBPath: dbPath,
			Client: client,
			JID:    jid,
		})
		fmt.Printf("connected %s as %s\n", filepath.Base(dbPath), jid.String())
	}

	if len(sessions) == 0 {
		fatalf("no logged-in sessions could be connected from %s", dbDir)
	}
	defer disconnectSessions(sessions)

	senderJID, err := parseSingleTarget(senderRaw)
	must(err, "parse sender JID")
	sender := findSenderSession(sessions, senderJID)
	if sender == nil {
		fatalf("sender %s not found in connected sessions", senderRaw)
	}

	helpers := filterHelpers(sessions, sender)
	if len(helpers) == 0 {
		fmt.Fprintf(os.Stderr, "warning: only one connected account found, no helper accounts available for typing state\n")
	}

	runMultiSendLoop(sender, helpers, targets, text, typingDuration, interval)

	waitForInterrupt("all clients connected")
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

func sendTextBatch(client *whatsmeow.Client, targets []types.JID, text string, interval time.Duration) {
	for i, target := range targets {
		if i > 0 && interval > 0 {
			fmt.Printf("waiting %s before next recipient...\n", interval)
			time.Sleep(interval)
		}

		resp, err := client.SendMessage(context.Background(), target, &waE2E.Message{
			Conversation: proto.String(text),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "send failed to %s: %v\n", target.String(), err)
			continue
		}

		fmt.Printf("message sent successfully\n")
		fmt.Printf("to: %s\n", target.String())
		fmt.Printf("id: %s\n", resp.ID)
		fmt.Printf("timestamp: %s\n", resp.Timestamp.Format(time.RFC3339))
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

func runSingleSendLoop(client *whatsmeow.Client, initialTargets []types.JID, text string, interval time.Duration) {
	targets := initialTargets
	for {
		prewarmTargets(client, targets)
		sendTextBatch(client, targets, text, interval)

		nextTargets, ok := promptNextTargets()
		if !ok {
			return
		}
		targets = nextTargets
	}
}

func runMultiSendLoop(sender *sessionClient, helpers []*sessionClient, initialTargets []types.JID, text string, typingDuration, interval time.Duration) {
	targets := initialTargets
	for {
		prewarmTargets(sender.Client, targets)

		for i, target := range targets {
			if i > 0 && interval > 0 {
				fmt.Printf("waiting %s before next recipient...\n", interval)
				time.Sleep(interval)
			}

			setHelpersTyping(helpers, sender.JID, types.ChatPresenceComposing)
			if err := sender.Client.SendChatPresence(context.Background(), target, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
				fmt.Fprintf(os.Stderr, "typing update failed from sender %s to %s: %v\n", sender.JID.String(), target.String(), err)
			}
			if typingDuration > 0 {
				fmt.Printf("helpers typing to sender %s, sender typing to %s for %s\n", sender.JID.String(), target.String(), typingDuration)
				time.Sleep(typingDuration)
			}

			resp, err := sender.Client.SendMessage(context.Background(), target, &waE2E.Message{
				Conversation: proto.String(text),
			})
			setHelpersTyping(helpers, sender.JID, types.ChatPresencePaused)
			if pauseErr := sender.Client.SendChatPresence(context.Background(), target, types.ChatPresencePaused, types.ChatPresenceMediaText); pauseErr != nil {
				fmt.Fprintf(os.Stderr, "paused update failed from sender %s to %s: %v\n", sender.JID.String(), target.String(), pauseErr)
			}

			if err != nil {
				fmt.Fprintf(os.Stderr, "send failed from %s to %s: %v\n", sender.JID.String(), target.String(), err)
				continue
			}
			fmt.Printf("message sent successfully\n")
			fmt.Printf("from: %s\n", sender.JID.String())
			fmt.Printf("to: %s\n", target.String())
			fmt.Printf("id: %s\n", resp.ID)
			fmt.Printf("timestamp: %s\n", resp.Timestamp.Format(time.RFC3339))
		}

		nextTargets, ok := promptNextTargets()
		if !ok {
			return
		}
		targets = nextTargets
	}
}

func promptNextTargets() ([]types.JID, bool) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("next to array (comma-separated, empty or exit to stop): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "read next to array failed: %v\n", err)
			return nil, false
		}

		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "exit") {
			return nil, false
		}

		targets, err := parseTargetJIDs(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid to array: %v\n", err)
			continue
		}
		return targets, true
	}
}

func prewarmTargets(client *whatsmeow.Client, targets []types.JID) {
	uniqueTargets := uniqueJIDs(targets)
	if len(uniqueTargets) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	fmt.Printf("prewarming %d target(s)...\n", len(uniqueTargets))

	if _, err := client.GetUserDevices(ctx, uniqueTargets); err != nil {
		fmt.Fprintf(os.Stderr, "prewarm devices failed: %v\n", err)
	} else {
		fmt.Printf("prewarm devices done\n")
	}

	if _, err := client.GetUserInfo(ctx, uniqueTargets); err != nil {
		fmt.Fprintf(os.Stderr, "prewarm user info failed: %v\n", err)
	} else {
		fmt.Printf("prewarm user info done\n")
	}
}

func ensureConnected(client *whatsmeow.Client, timeout time.Duration, dbPath string, allowQR bool) error {
	needsInitialSyncWait := client.Store.ID == nil
	monitor := newConnectMonitor(client)

	if client.Store.ID == nil {
		if !allowQR {
			return fmt.Errorf("no stored login session")
		}
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

func setHelpersTyping(helpers []*sessionClient, senderJID types.JID, state types.ChatPresence) {
	for _, helper := range helpers {
		if err := helper.Client.SendChatPresence(context.Background(), senderJID, state, types.ChatPresenceMediaText); err != nil {
			fmt.Fprintf(os.Stderr, "typing update failed from %s to %s: %v\n", helper.JID.String(), senderJID.String(), err)
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

func findSenderSession(sessions []*sessionClient, sender types.JID) *sessionClient {
	for _, session := range sessions {
		if sameAccount(session.JID, sender) {
			return session
		}
	}
	return nil
}

func filterHelpers(sessions []*sessionClient, sender *sessionClient) []*sessionClient {
	helpers := make([]*sessionClient, 0, len(sessions)-1)
	for _, session := range sessions {
		if session == sender {
			continue
		}
		helpers = append(helpers, session)
	}
	return helpers
}

func disconnectSessions(sessions []*sessionClient) {
	for _, session := range sessions {
		session.Client.Disconnect()
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

func sameAccount(a, b types.JID) bool {
	if a.IsEmpty() || b.IsEmpty() {
		return false
	}
	if a.ToNonAD().String() == b.ToNonAD().String() {
		return true
	}
	return a.User == b.User && a.Server == b.Server
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

func waitForInterrupt(msg string) {
	fmt.Printf("%s, press Ctrl+C to exit\n", msg)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	<-sigCh
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
