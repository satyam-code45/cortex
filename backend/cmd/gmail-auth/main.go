// Command gmail-auth performs the one-time Gmail OAuth authorization and caches
// the resulting refresh token.
//
// Run it once per machine:
//
//	make gmail-auth
//
// It starts a listener on a loopback port, prints a consent URL, and waits for
// Google to redirect the browser back with an authorization code. The code is
// exchanged for a refresh token, which is written to .gmail-token.json (mode
// 0600, gitignored). The server and the seeder read that file; neither can
// perform this flow, because it needs a human at a browser.
//
// The redirect is deliberately loopback rather than a hosted callback: nothing
// about Cortex is deployed, and a loopback redirect means the authorization
// code never leaves the machine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"cortex/internal/config"
	"cortex/internal/tools/gmail"
)

const (
	// authTimeout bounds how long the command waits for the browser round trip.
	authTimeout = 5 * time.Minute
	// shutdownTimeout lets the success page finish rendering before the
	// listener closes under it.
	shutdownTimeout = 2 * time.Second
	// readHeaderTimeout guards the loopback listener against a slow client.
	readHeaderTimeout = 10 * time.Second
)

// scopes requested at consent.
//
// readonly is all the agent tools need. insert and labels exist solely for the
// seeder: insert places backdated fixtures in the mailbox, and labels creates
// the label that GMAIL_QUERY_SCOPE can later narrow a graded run to.
var scopes = []string{
	gmail.ScopeReadonly,
	gmail.ScopeInsert,
	gmail.ScopeLabels,
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Resolved against the repository root, so these work whether the command is
	// run from backend/ (which is what `make gmail-auth` does) or from the root.
	credentialsPath := flag.String("credentials",
		config.RepoPath(envOr("GMAIL_CREDENTIALS_JSON", config.DefaultGmailCredentialsPath)),
		"path to the OAuth Desktop client JSON downloaded from Google Cloud")
	tokenPath := flag.String("token",
		config.RepoPath(envOr("GMAIL_TOKEN_PATH", config.DefaultGmailTokenPath)),
		"where to cache the refresh token")
	noBrowser := flag.Bool("no-browser", false,
		"print the consent URL instead of trying to open a browser")
	flag.Parse()

	if err := run(logger, *credentialsPath, *tokenPath, *noBrowser); err != nil {
		logger.Error("gmail authorization failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, credentialsPath, tokenPath string, noBrowser bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	creds, err := gmail.LoadCredentials(credentialsPath)
	if err != nil {
		return err
	}

	// Port 0 lets the OS pick. Google permits any port on a loopback redirect
	// for a Desktop client, so nothing has to be registered in advance.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	defer listener.Close() //nolint:errcheck // best-effort close on shutdown

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)

	pkce, err := gmail.NewPKCE()
	if err != nil {
		return err
	}
	state, err := gmail.RandomState()
	if err != nil {
		return err
	}

	authURL := creds.AuthCodeURL(redirectURI, state, pkce, scopes)
	fmt.Println()
	fmt.Println("Open this URL to authorize Cortex to read your Gmail:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	if !noBrowser {
		openBrowser(logger, authURL)
	}
	fmt.Println("Waiting for the redirect…")

	code, err := waitForCode(ctx, listener, state)
	if err != nil {
		return err
	}

	token, err := gmail.ExchangeCode(ctx, creds, code, redirectURI, pkce, nil)
	if err != nil {
		return err
	}
	if err := gmail.SaveToken(tokenPath, token); err != nil {
		return err
	}

	absolute, err := filepath.Abs(tokenPath)
	if err != nil {
		absolute = tokenPath
	}
	// The token itself is never printed: it is a long-lived credential to the
	// mailbox, and a terminal scrollback is not where it should live.
	logger.Info("gmail authorized", "token_file", absolute, "scopes", strings.Join(scopes, " "))
	fmt.Println()
	fmt.Println("Authorized. Refresh token cached at " + absolute + " (mode 0600).")
	return nil
}

// codeResult is what the loopback handler produces.
type codeResult struct {
	code string
	err  error
}

// waitForCode serves the redirect and returns the authorization code.
func waitForCode(ctx context.Context, listener net.Listener, wantState string) (string, error) {
	results := make(chan codeResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		// The state check is what stops another local process — or a stray tab
		// — from feeding this listener an authorization code from a different
		// consent flow.
		if got := query.Get("state"); got != wantState {
			writePage(w, http.StatusBadRequest, "Authorization failed",
				"The redirect did not carry the expected state value. Nothing was saved. Run `make gmail-auth` again.")
			send(results, codeResult{err: errors.New("gmail: redirect state did not match; the flow was not completed by this process")})
			return
		}
		if errCode := query.Get("error"); errCode != "" {
			writePage(w, http.StatusBadRequest, "Authorization declined",
				"Google reported: "+errCode+". Nothing was saved.")
			send(results, codeResult{err: fmt.Errorf("gmail: authorization declined: %s", errCode)})
			return
		}
		code := query.Get("code")
		if code == "" {
			writePage(w, http.StatusBadRequest, "Authorization failed",
				"The redirect carried no authorization code. Nothing was saved.")
			send(results, codeResult{err: errors.New("gmail: redirect carried no authorization code")})
			return
		}

		writePage(w, http.StatusOK, "Cortex is authorized",
			"You can close this tab and return to the terminal.")
		send(results, codeResult{code: code})
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			send(results, codeResult{err: fmt.Errorf("loopback server: %w", err)})
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	select {
	case result := <-results:
		return result.code, result.err
	case <-ctx.Done():
		return "", fmt.Errorf("gmail: gave up waiting for the browser redirect: %w", ctx.Err())
	}
}

// send delivers a result without blocking if one is already queued.
func send(ch chan<- codeResult, result codeResult) {
	select {
	case ch <- result:
	default:
	}
}

// writePage renders a minimal response for the browser tab.
//
// Both fields are escaped. `detail` can carry Google's `error` query parameter
// verbatim, and while the state check above gates every path that reaches it,
// that ordering is the only thing standing between this and script execution on
// the http://127.0.0.1:<port> origin. Escaping does not depend on one branch
// staying above another.
func writePage(w http.ResponseWriter, status int, heading, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>`+
		`<body style="font:16px system-ui;margin:4rem auto;max-width:36rem;color:#111">`+
		`<h1 style="font-size:1.25rem">%s</h1><p>%s</p></body>`,
		html.EscapeString(heading), html.EscapeString(heading), html.EscapeString(detail))
}

// openBrowser makes a best-effort attempt to open the consent URL.
//
// Failure is not an error: the URL is already printed, and a headless or
// remote shell is a perfectly normal place to run this.
func openBrowser(logger *slog.Logger, url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		logger.Debug("could not open a browser automatically", "error", err)
		return
	}
	// Reaped in the background so the command does not leave a zombie.
	go func() { _ = cmd.Wait() }()
}

// envOr returns the value of key, or def when the variable is unset or empty.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
