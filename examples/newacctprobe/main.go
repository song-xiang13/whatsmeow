package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

const newAccountQueryID = "25808450525504568"

type graphQLResponse struct {
	Users []graphQLUser `json:"xwa2_fetch_wa_users"`
}

type graphQLUser struct {
	ID                   string                `json:"id"`
	IntegritySignalsInfo *integritySignalsInfo `json:"integrity_signals_info"`
}

type integritySignalsInfo struct {
	TypeName     string `json:"__typename"`
	IsNewAccount *bool  `json:"is_new_account"`
}

type queryInput struct {
	JID              string         `json:"jid"`
	IntegritySignals map[string]any `json:"integrity_signals"`
}

type resultRow struct {
	JID          string
	IsNewAccount *bool
	TypeName     string
}

type persistedOutput struct {
	Summary outputSummary `json:"summary"`
	Rows    []resultRow   `json:"rows"`
	Errors  []string      `json:"errors,omitempty"`
}

type outputSummary struct {
	Rows   int `json:"rows"`
	True   int `json:"true"`
	False  int `json:"false"`
	Null   int `json:"null"`
	Errors int `json:"errors"`
}

func main() {
	dbFlag := flag.String("db", "", "Path to the SQLite session database")
	idsFileFlag := flag.String("ids-file", "", "Path to a text file that contains phone numbers")
	configFlag := flag.String("config", "", "Optional client config JSON file")
	socks5Flag := flag.String("socks5", "", "Optional SOCKS5 proxy URL")
	outputFlag := flag.String("out", "", "Optional local output file (.json, .csv, .tsv, .txt)")
	batchSizeFlag := flag.Int("batch-size", 20, "How many IDs to query in a single MEX request")
	limitFlag := flag.Int("limit", 0, "Optional max number of IDs to query, 0 means all")
	timeoutFlag := flag.Duration("connect-timeout", 2*time.Minute, "Maximum time to wait for login")
	logLevelFlag := flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
	flag.Parse()

	if *dbFlag == "" || *idsFileFlag == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *batchSizeFlag <= 0 {
		fatalf("batch-size must be > 0")
	}

	ids, err := extractNumbersFromFile(*idsFileFlag)
	must(err, "extract numbers")
	if *limitFlag > 0 && *limitFlag < len(ids) {
		ids = ids[:*limitFlag]
	}
	if len(ids) == 0 {
		fatalf("no IDs found in %s", *idsFileFlag)
	}

	client, err := openClient(*dbFlag, *socks5Flag, *logLevelFlag, *configFlag)
	must(err, "open client")
	defer client.Disconnect()

	must(ensureConnected(client, *timeoutFlag), "connect WhatsApp client")

	rows, errs := queryInBatches(client, ids, *batchSizeFlag)
	summary := buildSummary(rows, errs)
	printSummary(summary, rows, errs)
	if strings.TrimSpace(*outputFlag) != "" {
		must(writeOutputFile(*outputFlag, summary, rows, errs), "write output file")
		fmt.Printf("saved output to %s\n", *outputFlag)
	}
}

func extractNumbersFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)

	quotedToRe := regexp.MustCompile(`-to\s+"([0-9,]+)"`)
	lineListRe := regexp.MustCompile(`(?m)^\s*([0-9]+(?:,[0-9]+)+)\s*$`)
	singleLineRe := regexp.MustCompile(`(?m)^\s*([0-9]{6,})\s*$`)

	seen := make(map[string]struct{})
	var ids []string
	addCSV := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			ids = append(ids, part)
		}
	}

	for _, m := range quotedToRe.FindAllStringSubmatch(content, -1) {
		addCSV(m[1])
	}
	for _, m := range lineListRe.FindAllStringSubmatch(content, -1) {
		addCSV(m[1])
	}
	for _, m := range singleLineRe.FindAllStringSubmatch(content, -1) {
		addCSV(m[1])
	}

	return ids, nil
}

func queryInBatches(client *whatsmeow.Client, ids []string, batchSize int) ([]resultRow, []error) {
	ctx := context.Background()
	internal := client.DangerousInternals()
	rows := make([]resultRow, 0, len(ids))
	var errs []error

	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		batch := ids[start:end]
		fmt.Printf("querying batch %d-%d of %d\n", start+1, end, len(ids))

		input := make([]queryInput, 0, len(batch))
		for _, number := range batch {
			jid := types.NewJID(number, types.DefaultUserServer)
			input = append(input, queryInput{
				JID: jid.String(),
				IntegritySignals: map[string]any{
					"use_case": "START_CHAT_CONTEXT",
				},
			})
		}

		raw, err := internal.SendMexIQ(ctx, newAccountQueryID, map[string]any{
			"input": map[string]any{
				"query_input": input,
				"telemetry": map[string]any{
					"context": "INTERACTIVE",
				},
			},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("batch %d-%d: %w", start+1, end, err))
			continue
		}

		var resp graphQLResponse
		if err = json.Unmarshal(raw, &resp); err != nil {
			errs = append(errs, fmt.Errorf("batch %d-%d unmarshal: %w", start+1, end, err))
			continue
		}

		for i, user := range resp.Users {
			jid := ""
			if i < len(batch) {
				jid = types.NewJID(batch[i], types.DefaultUserServer).String()
			}
			if user.ID != "" {
				jid = user.ID
			}
			row := resultRow{JID: jid}
			if user.IntegritySignalsInfo != nil {
				row.TypeName = user.IntegritySignalsInfo.TypeName
				row.IsNewAccount = user.IntegritySignalsInfo.IsNewAccount
			}
			rows = append(rows, row)
		}
	}

	slices.SortFunc(rows, func(a, b resultRow) int {
		return strings.Compare(a.JID, b.JID)
	})
	return rows, errs
}

func buildSummary(rows []resultRow, errs []error) outputSummary {
	var trueCount, falseCount, nullCount int
	for _, row := range rows {
		switch {
		case row.IsNewAccount == nil:
			nullCount++
		case *row.IsNewAccount:
			trueCount++
		default:
			falseCount++
		}
	}

	return outputSummary{
		Rows:   len(rows),
		True:   trueCount,
		False:  falseCount,
		Null:   nullCount,
		Errors: len(errs),
	}
}

func printSummary(summary outputSummary, rows []resultRow, errs []error) {
	fmt.Printf("rows=%d true=%d false=%d null=%d errors=%d\n", summary.Rows, summary.True, summary.False, summary.Null, summary.Errors)
	for _, row := range rows {
		value := "null"
		if row.IsNewAccount != nil {
			value = fmt.Sprintf("%v", *row.IsNewAccount)
		}
		fmt.Printf("%s\t%s\t%s\n", row.JID, value, row.TypeName)
	}
	if len(errs) > 0 {
		fmt.Println("errors:")
		for _, err := range errs {
			fmt.Println(err)
		}
	}
}

func writeOutputFile(path string, summary outputSummary, rows []resultRow, errs []error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && !os.IsExist(err) {
		return err
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		return writeJSONOutput(path, summary, rows, errs)
	case ".csv":
		return writeDelimitedOutput(path, summary, rows, errs, ',')
	case ".tsv", ".txt", "":
		return writeDelimitedOutput(path, summary, rows, errs, '\t')
	default:
		return fmt.Errorf("unsupported output extension %q (use .json, .csv, .tsv or .txt)", ext)
	}
}

func writeJSONOutput(path string, summary outputSummary, rows []resultRow, errs []error) error {
	payload := persistedOutput{
		Summary: summary,
		Rows:    rows,
	}
	if len(errs) > 0 {
		payload.Errors = make([]string, 0, len(errs))
		for _, err := range errs {
			payload.Errors = append(payload.Errors, err.Error())
		}
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func writeDelimitedOutput(path string, summary outputSummary, rows []resultRow, errs []error, delimiter rune) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	writer.Comma = delimiter
	if err = writer.Write([]string{"jid", "is_new_account", "type_name"}); err != nil {
		return err
	}
	for _, row := range rows {
		value := "null"
		if row.IsNewAccount != nil {
			value = fmt.Sprintf("%v", *row.IsNewAccount)
		}
		if err = writer.Write([]string{row.JID, value, row.TypeName}); err != nil {
			return err
		}
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return err
	}

	if len(errs) == 0 {
		return nil
	}

	if delimiter == '\t' {
		if _, err = fmt.Fprintf(file, "\n# summary\trows=%d\ttrue=%d\tfalse=%d\tnull=%d\terrors=%d\n", summary.Rows, summary.True, summary.False, summary.Null, summary.Errors); err != nil {
			return err
		}
		if _, err = fmt.Fprintln(file, "# errors"); err != nil {
			return err
		}
		for _, item := range errs {
			if _, err = fmt.Fprintf(file, "# %s\n", item.Error()); err != nil {
				return err
			}
		}
		return nil
	}

	if err = writer.Write([]string{}); err != nil {
		return err
	}
	if err = writer.Write([]string{"summary", fmt.Sprintf("rows=%d", summary.Rows), fmt.Sprintf("true=%d", summary.True), fmt.Sprintf("false=%d", summary.False), fmt.Sprintf("null=%d", summary.Null), fmt.Sprintf("errors=%d", summary.Errors)}); err != nil {
		return err
	}
	for _, item := range errs {
		if err = writer.Write([]string{"error", item.Error()}); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func init() {
	flag.CommandLine.SetOutput(os.Stdout)
}
