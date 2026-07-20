package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/daemon"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

type chatOptions struct {
	agentID      string
	session      string
	query        string
	baseURL      string
	apiKey       string
	continueLast bool
}

type cliAgent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"createdAt"`
}

type cliSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	UpdatedAt int64  `json:"updatedAt"`
}

type chatClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func chatCmd() *cobra.Command {
	var opts chatOptions
	cmd := &cobra.Command{
		Use:   "chat [message]",
		Short: "Chat with a FastClaw agent in the terminal",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.query == "" && len(args) > 0 {
				opts.query = strings.Join(args, " ")
			}
			return runChat(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVarP(&opts.agentID, "agent", "a", "", "agent ID or name")
	cmd.Flags().StringVarP(&opts.session, "resume", "r", "", "resume a session by ID")
	cmd.Flags().BoolVarP(&opts.continueLast, "continue", "c", false, "continue the most recent session")
	cmd.Flags().StringVarP(&opts.query, "query", "q", "", "send one message and exit")
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "gateway URL (default http://127.0.0.1:$FASTCLAW_PORT)")
	cmd.Flags().StringVar(&opts.apiKey, "api-key", "", "API key (or FASTCLAW_API_KEY)")
	return cmd
}

func isInteractiveTerminal(in, out *os.File) bool {
	inStat, inErr := in.Stat()
	outStat, outErr := out.Stat()
	return inErr == nil && outErr == nil && inStat.Mode()&os.ModeCharDevice != 0 && outStat.Mode()&os.ModeCharDevice != 0
}

func runChat(ctx context.Context, opts chatOptions) error {
	env := config.LoadEnv()
	port := env.Gateway.Port
	if port == 0 {
		port = 18953
	}
	localGateway := opts.baseURL == ""
	if opts.baseURL == "" {
		opts.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	opts.baseURL = strings.TrimRight(opts.baseURL, "/")
	if opts.apiKey == "" {
		opts.apiKey = os.Getenv("FASTCLAW_API_KEY")
	}
	if opts.query == "" && !isInteractiveTerminal(os.Stdin, os.Stdout) {
		if data, err := io.ReadAll(os.Stdin); err == nil {
			opts.query = strings.TrimSpace(string(data))
		}
	}

	if localGateway {
		if err := ensureGateway(ctx, opts.baseURL, port); err != nil {
			return err
		}
	}
	if opts.apiKey == "" {
		if !localGateway {
			return errors.New("remote chat requires --api-key or FASTCLAW_API_KEY")
		}
		var err error
		opts.apiKey, err = ensureCLIToken(ctx)
		if err != nil {
			return err
		}
	}

	c := &chatClient{baseURL: opts.baseURL, apiKey: opts.apiKey, http: &http.Client{}}
	agents, err := c.agents(ctx)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	if len(agents) == 0 {
		return errors.New("no agents configured; create one with `fastclaw agents init <name>`")
	}
	agent, err := selectAgent(agents, opts.agentID)
	if err != nil {
		return err
	}
	if opts.continueLast && opts.session == "" {
		sessions, err := c.sessions(ctx, agent.ID)
		if err != nil {
			return err
		}
		if len(sessions) > 0 {
			opts.session = sessions[0].ID
		}
	}
	if opts.session == "" {
		opts.session = newCLISessionID()
	}

	if opts.query != "" {
		return c.stream(ctx, agent.ID, opts.session, opts.query, os.Stdout)
	}
	if !isInteractiveTerminal(os.Stdin, os.Stdout) {
		return errors.New("interactive chat requires a terminal; use --query or pipe a message as an argument")
	}
	return runChatREPL(ctx, c, agent, opts.session)
}

func ensureGateway(ctx context.Context, baseURL string, port int) error {
	if gatewayReady(ctx, baseURL) {
		return nil
	}
	st, _ := daemon.GetStatus()
	if st == nil || !st.Running {
		fmt.Fprintln(os.Stderr, "Starting FastClaw gateway…")
		if err := daemon.Start(port); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if gatewayReady(ctx, baseURL) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("gateway did not become ready at %s; check `fastclaw daemon logs`", baseURL)
}

func gatewayReady(ctx context.Context, baseURL string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func ensureCLIToken(ctx context.Context) (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "cli-token")
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	st, err := openStoreFromEnv()
	if err != nil {
		return "", err
	}
	defer st.Close()
	accounts, err := users.NewAccounts(st)
	if err != nil {
		return "", err
	}
	list, err := accounts.List(ctx)
	if err != nil {
		return "", err
	}
	owner := ""
	for _, account := range list {
		if account.Role == users.RoleSuperAdmin {
			owner = account.ID
			break
		}
	}
	if owner == "" {
		return "", errors.New("no super_admin found; finish FastClaw onboarding first")
	}
	keys, err := users.NewAPIKeys(st)
	if err != nil {
		return "", err
	}
	_, token, err := keys.Create(ctx, owner, "FastClaw terminal", users.APIKeyTypeUser, nil)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create terminal credential directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("save terminal credential: %w", err)
	}
	return token, nil
}

func (c *chatClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *chatClient) agents(ctx context.Context) ([]cliAgent, error) {
	resp, err := c.request(ctx, http.MethodGet, "/api/agents", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var payload struct {
		Agents []cliAgent `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Agents, nil
}

func (c *chatClient) sessions(ctx context.Context, agentID string) ([]cliSession, error) {
	resp, err := c.request(ctx, http.MethodGet, "/api/chat/sessions?agentId="+agentID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var payload struct {
		Sessions []cliSession `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	sort.SliceStable(payload.Sessions, func(i, j int) bool { return payload.Sessions[i].UpdatedAt > payload.Sessions[j].UpdatedAt })
	return payload.Sessions, nil
}

func selectAgent(agents []cliAgent, wanted string) (cliAgent, error) {
	if wanted == "" {
		// The API currently returns agents newest-first. The terminal's
		// implicit default is the user's first-created agent, independent of
		// the response ordering. Older servers may omit createdAt; preserve
		// their response order in that case.
		selected := agents[0]
		for _, agent := range agents[1:] {
			if !agent.CreatedAt.IsZero() && (selected.CreatedAt.IsZero() || agent.CreatedAt.Before(selected.CreatedAt)) {
				selected = agent
			}
		}
		return selected, nil
	}
	for _, agent := range agents {
		if agent.ID == wanted || strings.EqualFold(agent.Name, wanted) {
			return agent, nil
		}
	}
	return cliAgent{}, fmt.Errorf("agent %q not found", wanted)
}

func runChatREPL(ctx context.Context, c *chatClient, agent cliAgent, sessionID string) error {
	fmt.Printf("\n\033[1;36mFastClaw\033[0m · agent: \033[1m%s\033[0m", agent.Name)
	if agent.Model != "" {
		fmt.Printf(" · model: %s", agent.Model)
	}
	fmt.Printf("\nWeb: %s\n", c.baseURL)
	fmt.Println("Type /help for commands.")
	fmt.Println()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for {
		fmt.Printf("%sYou >%s ", ansiIf(true, "\033[1;32m"), ansiIf(true, "\033[0m"))
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		switch {
		case text == "/exit" || text == "/quit":
			return nil
		case text == "/help":
			fmt.Println("/new  start a new chat    /sessions  list chats    /exit  quit")
			continue
		case text == "/new":
			sessionID = newCLISessionID()
			fmt.Println("Started a new chat.")
			continue
		case text == "/sessions":
			sessions, err := c.sessions(ctx, agent.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sessions: %v\n", err)
				continue
			}
			for i, session := range sessions {
				if i == 10 {
					break
				}
				label := session.Title
				if label == "" {
					label = session.Preview
				}
				fmt.Printf("  %s  %s\n", session.ID, label)
			}
			continue
		}
		fmt.Printf("%sAgent >%s ", ansiIf(true, "\033[1;35m"), ansiIf(true, "\033[0m"))
		if err := c.stream(ctx, agent.ID, sessionID, text, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
	}
}

func (c *chatClient) stream(ctx context.Context, agentID, sessionID, message string, out io.Writer) error {
	resp, err := c.request(ctx, http.MethodPost, "/api/chat/stream", map[string]any{
		"agentId": agentID, "sessionId": sessionID, "message": message,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	interactive := shouldRenderMarkdown(out)
	statusVisible := false
	wrote := false
	atLineStart := true
	streamedContent := false
	var markdownBuffer strings.Builder
	toolNames := make(map[string]string)
	// Give immediate feedback before the provider emits its first token. The
	// status occupies one replaceable terminal line; pipes/files stay clean.
	if interactive {
		fmt.Fprintf(out, "%s◌ Thinking…%s", ansiIf(true, "\033[36m"), ansiIf(true, "\033[0m"))
		statusVisible = true
	}
	clearStatus := func() {
		if statusVisible {
			fmt.Fprint(out, "\r\033[2K")
			statusVisible = false
		}
	}
	writeText := func(text string) {
		if text == "" {
			return
		}
		clearStatus()
		fmt.Fprint(out, text)
		wrote = true
		atLineStart = strings.HasSuffix(text, "\n")
	}
	flushMarkdown := func(force bool) {
		if !interactive || markdownBuffer.Len() == 0 {
			return
		}
		text := markdownBuffer.String()
		cut := len(text)
		if !force {
			cut = completeMarkdownPrefix(text)
			if cut == 0 {
				return
			}
		}
		block := text[:cut]
		markdownBuffer.Reset()
		markdownBuffer.WriteString(text[cut:])
		rendered, err := renderTerminalMarkdown(block)
		if err != nil {
			writeText(block)
			return
		}
		writeText(rendered)
	}
	writeContent := func(text string) {
		if !interactive {
			writeText(text)
			return
		}
		markdownBuffer.WriteString(text)
		flushMarkdown(false)
	}
	startLine := func() {
		flushMarkdown(true)
		clearStatus()
		if wrote && !atLineStart {
			fmt.Fprintln(out)
		}
		atLineStart = true
	}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch event.Type {
		case "content_delta":
			delta, _ := event.Data["delta"].(string)
			// Write every provider delta as it arrives. Buffering until done made
			// the endpoint streaming in name only from a terminal user's view.
			writeContent(delta)
			streamedContent = streamedContent || delta != ""
		case "content":
			if !streamedContent {
				text, _ := event.Data["content"].(string)
				writeContent(text)
			}
			streamedContent = false
		case "tool_call":
			name, _ := event.Data["name"].(string)
			id, _ := event.Data["id"].(string)
			if id != "" && name != "" {
				toolNames[id] = name
			}
			if name != "" {
				startLine()
				fmt.Fprintf(out, "%s↳ %s%s\n", ansiIf(interactive, "\033[36m"), name, ansiIf(interactive, "\033[0m"))
				wrote, atLineStart = true, true
				streamedContent = false
			}
		case "tool_result":
			id, _ := event.Data["id"].(string)
			name := toolNames[id]
			result, _ := event.Data["result"].(string)
			startLine()
			label := "done"
			if name != "" {
				label = name
			}
			fmt.Fprintf(out, "%s✓ %s%s", ansiIf(interactive, "\033[32m"), label, ansiIf(interactive, "\033[0m"))
			if summary := toolResultSummary(result); summary != "" {
				fmt.Fprintf(out, " %s%s%s", ansiIf(interactive, "\033[2m"), summary, ansiIf(interactive, "\033[0m"))
			}
			fmt.Fprintln(out)
			wrote, atLineStart = true, true
		case "error":
			msg, _ := event.Data["message"].(string)
			return errors.New(msg)
		case "done":
			flushMarkdown(true)
			clearStatus()
			if wrote && !atLineStart {
				fmt.Fprintln(out)
			}
			return nil
		}
	}
	clearStatus()
	flushMarkdown(true)
	if wrote && !atLineStart {
		fmt.Fprintln(out)
	}
	return scanner.Err()
}

// completeMarkdownPrefix returns the portion of a streamed response that is
// safe to render without guessing how an unfinished Markdown construct ends.
// A blank line closes normal blocks (paragraphs, lists, tables); fenced code
// is held until its closing fence arrives, even when it contains blank lines.
func completeMarkdownPrefix(text string) int {
	inFence := false
	lastComplete := 0
	lineStart := 0
	for lineStart < len(text) {
		relEnd := strings.IndexByte(text[lineStart:], '\n')
		if relEnd < 0 {
			break
		}
		lineEnd := lineStart + relEnd
		line := strings.TrimSpace(text[lineStart:lineEnd])
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			if !inFence {
				lastComplete = lineEnd + 1
			}
		} else if !inFence && line == "" {
			lastComplete = lineEnd + 1
		}
		lineStart = lineEnd + 1
	}
	return lastComplete
}

func ansiIf(enabled bool, code string) string {
	if enabled && os.Getenv("NO_COLOR") == "" {
		return code
	}
	return ""
}

func toolResultSummary(result string) string {
	result = strings.Join(strings.Fields(result), " ")
	if result == "" {
		return ""
	}
	const max = 96
	if len(result) > max {
		return result[:max-1] + "…"
	}
	return result
}

func shouldRenderMarkdown(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	return err == nil && stat.Mode()&os.ModeCharDevice != 0
}

func renderTerminalMarkdown(markdown string) (string, error) {
	style := "dark"
	// COLORFGBG's final field is the terminal background colour (0–6 are
	// conventionally dark, 7–15 light). This keeps Glamour readable in light
	// terminals instead of always forcing its dark-background palette.
	if fields := strings.Split(os.Getenv("COLORFGBG"), ";"); len(fields) > 0 {
		if bg, err := strconv.Atoi(fields[len(fields)-1]); err == nil && bg >= 7 {
			style = "light"
		}
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(100),
	)
	if err != nil {
		return "", err
	}
	return renderer.Render(markdown)
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, payload.Error)
	}
	return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

func newCLISessionID() string {
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("cli-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(suffix[:]))
}
