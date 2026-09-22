// gemini-web-cli drives Gemini through an already-running Chrome CDP endpoint.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

const (
	geminiURL       = "https://gemini.google.com/app"
	defaultCDPURL   = "http://localhost:9222"
	responseTimeout = 120 * time.Second
)

var (
	promptSelectors   = []string{"textarea", "[contenteditable='true'][role='textbox']", "[contenteditable='true']", "[role='textbox']"}
	responseSelectors = []string{"model-response", "[data-message-author-role='assistant']", "[data-testid='assistant-message']"}
	debugEnabled      bool
	debugMu           sync.Mutex
)

func debugf(format string, args ...any) {
	if !debugEnabled {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	fmt.Fprintf(os.Stderr, "%s DEBUG "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

type loginRequiredError struct{ message string }

func (e *loginRequiredError) Error() string { return e.message }

// Browser owns only the CDP connection. It never launches or closes Chrome.
type Browser struct {
	allocatorCtx context.Context
	cancel       context.CancelFunc
}

func newBrowser(cdpURL string) *Browser {
	debugf("creating CDP connection to %s", cdpURL)
	ctx, cancel := chromedp.NewRemoteAllocator(context.Background(), cdpURL)
	return &Browser{allocatorCtx: ctx, cancel: cancel}
}

func (b *Browser) newPage() (context.Context, context.CancelFunc, error) {
	ctx, cancel := chromedp.NewContext(b.allocatorCtx)
	// Run the first action on the session context itself. Running it on a
	// short-lived child context would make chromedp bind the new target to that
	// child and close the target when its timeout expires.
	debugf("opening a Gemini tab")
	err := chromedp.Run(ctx, chromedp.Navigate(geminiURL), chromedp.Sleep(750*time.Millisecond))
	if err != nil {
		debugf("Gemini navigation failed: %v", err)
		cancel()
		return nil, nil, err
	}
	if err := ensureAnonymous(ctx); err != nil {
		debugf("anonymous access check failed: %v", err)
		cancel()
		return nil, nil, err
	}
	if _, err := findPromptSelector(ctx); err != nil {
		debugf("prompt box check failed: %v", err)
		cancel()
		return nil, nil, err
	}
	return ctx, cancel, nil
}

func (b *Browser) close() { b.cancel() }

func javascriptString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func ensureAnonymous(ctx context.Context) error {
	var state struct {
		URL  string `json:"url"`
		Text string `json:"text"`
	}
	expr := `({url: location.href, text: (document.body && document.body.innerText || "").toLowerCase()})`
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &state)); err != nil {
		return err
	}
	url := strings.ToLower(state.URL)
	if strings.Contains(url, "accounts.google.com") || strings.Contains(url, "signin") {
		debugf("Google sign-in redirect detected: %s", state.URL)
		return &loginRequiredError{"Gemini redirected to Google sign-in."}
	}
	for _, phrase := range []string{"sign in to continue", "sign in to use gemini", "you need to sign in", "login to continue"} {
		if strings.Contains(state.Text, phrase) {
			debugf("Gemini sign-in gate detected: %q", phrase)
			return &loginRequiredError{"Gemini requires sign-in for this session."}
		}
	}
	return nil
}

func findPromptSelector(ctx context.Context) (string, error) {
	var selector string
	encoded, _ := json.Marshal(promptSelectors)
	expr := `(() => { for (const s of ` + string(encoded) + `) { const els = document.querySelectorAll(s); const el = els[els.length - 1]; if (el && el.getClientRects().length && getComputedStyle(el).visibility !== "hidden") return s; } return ""; })()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &selector)); err != nil {
		return "", err
	}
	if selector == "" {
		return "", errors.New("could not find Gemini's prompt box; its page structure may have changed")
	}
	debugf("found prompt selector %q", selector)
	return selector, nil
}

func lastResponse(ctx context.Context) (string, error) {
	var text string
	encoded, _ := json.Marshal(responseSelectors)
	expr := `(() => { for (const s of ` + string(encoded) + `) { const els = document.querySelectorAll(s); const el = els[els.length - 1]; if (el && el.innerText && el.innerText.trim()) return el.innerText.trim(); } return ""; })()`
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &text))
	return text, err
}

func sendPrompt(ctx context.Context, prompt string) (string, error) {
	debugf("sending prompt (%d characters)", len([]rune(prompt)))
	if err := ensureAnonymous(ctx); err != nil {
		return "", err
	}
	previous, err := lastResponse(ctx)
	if err != nil {
		return "", err
	}
	selector, err := findPromptSelector(ctx)
	if err != nil {
		return "", err
	}
	// execCommand produces a trusted edit path in Chrome's contenteditable UI and
	// dispatches the input event Gemini uses to enable its send action.
	expr := `(() => { const el = document.querySelectorAll(` + javascriptString(selector) + `); const target = el[el.length - 1]; target.focus(); if (target instanceof HTMLTextAreaElement) { target.value = ` + javascriptString(prompt) + `; target.dispatchEvent(new Event("input", {bubbles:true})); } else if (!document.execCommand("insertText", false, ` + javascriptString(prompt) + `)) { target.textContent = ` + javascriptString(prompt) + `; target.dispatchEvent(new Event("input", {bubbles:true})); } return true; })()`
	var done bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &done)); err != nil {
		return "", err
	}
	debugf("inserted prompt text into %q", selector)

	// Gemini enables its Send button asynchronously after an input event. A
	// semantic click is more reliable than a synthetic Enter key on rich-text
	// inputs; retain Enter as a fallback for markup variants without that button.
	sent := false
	sendDeadline := time.Now().Add(2 * time.Second)
	for !sent && time.Now().Before(sendDeadline) {
		sendExpr := `(() => { const button = [...document.querySelectorAll("button")].find((b) => { const label = (b.getAttribute("aria-label") || "").toLowerCase(); return b.getClientRects().length && !b.disabled && label.includes("send") && !label.includes("feedback"); }); if (!button) return false; button.click(); return true; })()`
		if err := chromedp.Run(ctx, chromedp.Evaluate(sendExpr, &sent)); err != nil {
			return "", err
		}
		if !sent {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if !sent {
		debugf("no enabled Send button appeared; using Enter fallback")
		if err := chromedp.Run(ctx,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return input.DispatchKeyEvent(input.KeyDown).WithKey("Enter").WithCode("Enter").Do(ctx)
			}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				return input.DispatchKeyEvent(input.KeyUp).WithKey("Enter").WithCode("Enter").Do(ctx)
			}),
		); err != nil {
			return "", err
		}
	} else {
		debugf("submitted prompt by clicking Gemini Send button")
	}

	deadline := time.Now().Add(responseTimeout)
	answer := ""
	var unchangedAt time.Time
	for time.Now().Before(deadline) {
		if err := ensureAnonymous(ctx); err != nil {
			return "", err
		}
		current, err := lastResponse(ctx)
		if err != nil {
			return "", err
		}
		if current != "" && current != previous {
			if current != answer {
				answer, unchangedAt = current, time.Now()
				debugf("received response update (%d characters)", len([]rune(current)))
			} else if time.Since(unchangedAt) >= 1500*time.Millisecond {
				debugf("response stabilized")
				return answer, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if answer != "" {
		debugf("response timed out; returning partial response")
		return answer, nil
	}
	debugf("response timed out with no response")
	return "", errors.New("timed out waiting for Gemini's response")
}

func runREPL(cdpURL string) error {
	debugf("starting REPL")
	browser := newBrowser(cdpURL)
	defer browser.close()
	pageCtx, closePage, err := browser.newPage()
	if err != nil {
		return err
	}
	defer closePage()

	reader := bufio.NewScanner(os.Stdin)
	fmt.Println("Connected. Commands: /new, /exit")
	for {
		fmt.Print("you> ")
		if !reader.Scan() {
			return reader.Err()
		}
		line := reader.Text()
		prompt := strings.TrimSpace(line)
		switch {
		case prompt == "":
			continue
		case prompt == "/exit":
			return nil
		case prompt == "/new":
			closePage()
			pageCtx, closePage, err = browser.newPage()
			if err != nil {
				return err
			}
			fmt.Println("Started a new chat.")
		default:
			fmt.Println("gemini> waiting for response...")
			answer, err := sendPrompt(pageCtx, line)
			if err != nil {
				debugf("prompt failed: %v", err)
				return err
			}
			fmt.Printf("gemini> %s\n", answer)
		}
	}
}

type acpSession struct {
	pageCtx context.Context
	close   context.CancelFunc
	mu      sync.Mutex
	cancel  context.CancelFunc
}

type acpServer struct {
	browser  *Browser
	sessions map[string]*acpSession
	mu       sync.Mutex
	outputMu sync.Mutex
}

func newACPServer(cdpURL string) *acpServer {
	return &acpServer{browser: newBrowser(cdpURL), sessions: make(map[string]*acpSession)}
}

func (s *acpServer) write(message any) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(message)
}

func (s *acpServer) result(id any, value any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": value})
}
func (s *acpServer) rpcError(id any, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (s *acpServer) updateText(sessionID, text string) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": sessionID,
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": fmt.Sprintf("msg_%d", time.Now().UnixNano()), "content": map[string]string{"type": "text", "text": text}},
	}})
}

func contentText(value any) (string, error) {
	blocks, ok := value.([]any)
	if !ok {
		return "", errors.New("session/prompt requires a prompt array")
	}
	var parts []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		} else if block["type"] == "resource" {
			if resource, ok := block["resource"].(map[string]any); ok {
				if text, ok := resource["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return "", errors.New("the prompt did not contain text Gemini can receive")
	}
	return text, nil
}

func (s *acpServer) createSession() (string, error) {
	pageCtx, closePage, err := s.browser.newPage()
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("gemini_%d", time.Now().UnixNano())
	s.mu.Lock()
	s.sessions[id] = &acpSession{pageCtx: pageCtx, close: closePage}
	s.mu.Unlock()
	return id, nil
}

func (s *acpServer) getSession(id string) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *acpServer) handlePrompt(id any, params map[string]any) {
	sessionID, _ := params["sessionId"].(string)
	session := s.getSession(sessionID)
	if session == nil {
		s.rpcError(id, -32002, "Unknown ACP session")
		return
	}
	prompt, err := contentText(params["prompt"])
	if err != nil {
		s.rpcError(id, -32602, err.Error())
		return
	}

	session.mu.Lock()
	if session.cancel != nil {
		session.mu.Unlock()
		s.rpcError(id, -32003, "A prompt is already in progress")
		return
	}
	turnCtx, cancel := context.WithCancel(session.pageCtx)
	session.cancel = cancel
	session.mu.Unlock()
	defer func() { session.mu.Lock(); session.cancel = nil; session.mu.Unlock(); cancel() }()

	answer, err := sendPrompt(turnCtx, prompt)
	if errors.Is(err, context.Canceled) {
		s.result(id, map[string]string{"stopReason": "cancelled"})
		return
	}
	if err != nil {
		s.updateText(sessionID, "Gemini error: "+err.Error())
		s.result(id, map[string]string{"stopReason": "refusal"})
		return
	}
	s.updateText(sessionID, answer)
	s.result(id, map[string]string{"stopReason": "end_turn"})
}

func (s *acpServer) closeSession(id string) bool {
	s.mu.Lock()
	session := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	if session.cancel != nil {
		session.cancel()
	}
	session.close()
	session.mu.Unlock()
	return true
}

func (s *acpServer) handle(message map[string]any) {
	id, hasID := message["id"]
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "initialize":
		if hasID {
			s.result(id, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"promptCapabilities": map[string]any{}}, "agentInfo": map[string]string{"name": "gemini-web-cli", "title": "Gemini Web CLI", "version": "0.1.0"}, "authMethods": []any{}})
		}
	case "session/new":
		sessionID, err := s.createSession()
		if err != nil {
			s.rpcError(id, -32000, "Could not create Gemini session: "+err.Error())
		} else {
			s.result(id, map[string]string{"sessionId": sessionID})
		}
	case "session/prompt":
		s.handlePrompt(id, params)
	case "session/cancel":
		if session := s.getSession(stringParam(params, "sessionId")); session != nil {
			session.mu.Lock()
			if session.cancel != nil {
				session.cancel()
			}
			session.mu.Unlock()
		}
	case "session/close":
		if s.closeSession(stringParam(params, "sessionId")) {
			s.result(id, map[string]any{})
		} else {
			s.rpcError(id, -32002, "Unknown ACP session")
		}
	default:
		if hasID {
			s.rpcError(id, -32601, "Unsupported ACP method: "+method)
		}
	}
}

func stringParam(params map[string]any, key string) string {
	value, _ := params[key].(string)
	return value
}

func (s *acpServer) run() error {
	defer s.browser.close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 4*1024*1024)
	var work sync.WaitGroup
	for scanner.Scan() {
		var message map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			s.rpcError(nil, -32700, "Parse error: "+err.Error())
			continue
		}
		if message["jsonrpc"] != "2.0" {
			s.rpcError(message["id"], -32600, "Invalid JSON-RPC request")
			continue
		}
		work.Add(1)
		go func(m map[string]any) { defer work.Done(); s.handle(m) }(message)
	}
	work.Wait()
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.closeSession(id)
	}
	return scanner.Err()
}

func main() {
	cdpDefault := os.Getenv("CHROME_CDP_URL")
	if cdpDefault == "" {
		cdpDefault = defaultCDPURL
	}
	cdpURL := flag.String("cdp-url", cdpDefault, "Chrome DevTools endpoint")
	debug := flag.Bool("debug", os.Getenv("GEMINI_WEB_CLI_DEBUG") == "1", "Write browser diagnostics to stderr")
	flag.Parse()
	debugEnabled = *debug
	mode := "repl"
	if flag.NArg() > 0 {
		mode = flag.Arg(0)
	}
	var err error
	debugf("starting mode %q", mode)
	switch mode {
	case "repl":
		err = runREPL(*cdpURL)
	case "acp":
		err = newACPServer(*cdpURL).run()
	default:
		err = fmt.Errorf("unknown mode %q (use repl or acp)", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
