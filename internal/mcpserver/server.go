// Package mcpserver exposes a kb over the Model Context Protocol, on stdio
// (kb mcp) or streamable HTTP (kb mcp --http addr, or mounted at /mcp by kb
// serve --mcp). The tools are everything an agent needs to build and keep a
// Wikipedia-sized knowledge base without holding it in context: search and
// listings sorted by edit time, sectioned reads, conflict-checked edits, red
// links and other maintenance reports, files, and a task/notes/change feed
// to carry progress between sessions.
//
// # Who can reach it
//
// Every tool reads or writes the kb and nothing else: no tool touches the
// filesystem or the network. Over stdio the client launched the process.
// Over HTTP anyone who can reach the port can rewrite the kb, so a
// non-loopback bind requires a shared secret in KB_MCP_TOKEN.
package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xen0bit/kb/internal/store"
)

// TokenEnv holds the shared secret that gates HTTP. It is an environment
// variable rather than a flag so the secret is not in the process table.
const TokenEnv = "KB_MCP_TOKEN"

// LogLevelEnv sets the log level: debug, info, warn or error.
const LogLevelEnv = "KB_LOG_LEVEL"

// NewLogger builds a logger on stderr; over stdio, stdout is the wire.
func NewLogger(level slog.Level) *slog.Logger {
	switch strings.ToLower(os.Getenv(LogLevelEnv)) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// Options configures the server.
type Options struct {
	Version      string
	Logger       *slog.Logger
	MaxFileBytes int64  // upload limit; 0 means store.DefaultMaxFileBytes
	Author       string // default author when neither the call nor the client names one
}

// NewServer builds an MCP server over a store. The store serializes writes
// itself, so one server can be shared by every HTTP session.
func NewServer(st *store.Store, opts Options) *mcp.Server {
	if opts.Logger == nil {
		opts.Logger = NewLogger(slog.LevelWarn)
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = store.DefaultMaxFileBytes
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:        "kb",
		Title:       "kb",
		Description: "A Wikipedia-style markdown knowledge base with files, full history, search, red links, tasks and notes.",
		Version:     opts.Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
		Logger:       opts.Logger,
	})
	t := &tools{st: st, log: opts.Logger, maxFile: opts.MaxFileBytes, defAuthor: opts.Author}
	t.register(server)
	t.registerResources(server)
	return server
}

// Serve runs the server over stdio until the client disconnects.
func Serve(ctx context.Context, st *store.Store, opts Options) error {
	err := NewServer(st, opts).Run(ctx, &mcp.StdioTransport{})
	// The client closing stdin is how a stdio session ends: not an error.
	if err != nil && (errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF") || errors.Is(err, context.Canceled)) {
		return nil
	}
	return err
}

// Handler is the streamable-HTTP MCP endpoint, without auth or logging: the
// caller wraps it (see Protect).
func Handler(st *store.Store, opts Options) http.Handler {
	server := NewServer(st, opts)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Logger: opts.Logger})
}

// Listen opens addr and enforces the bind rule: a loopback address needs no
// token, anything else does.
func Listen(addr, token, what string) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	if token == "" && !isLoopback(l.Addr()) {
		_ = l.Close()
		return nil, fmt.Errorf("refusing to serve %s on %s without a shared secret: anyone who can reach this port could "+
			"read the kb, and over MCP rewrite it. Set %s to gate it, or bind a loopback address such as 127.0.0.1%s",
			what, l.Addr(), TokenEnv, portOf(addr))
	}
	return l, nil
}

// ServeHTTP serves MCP alone at /mcp (and /) on addr.
func ServeHTTP(ctx context.Context, addr string, st *store.Store, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = NewLogger(slog.LevelInfo)
	}
	token := os.Getenv(TokenEnv)
	l, err := Listen(addr, token, "MCP")
	if err != nil {
		return err
	}
	h := Handler(st, opts)
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.Handle("/", h)
	auth := "loopback, no token"
	if token != "" {
		auth = "bearer token"
	}
	opts.Logger.Info("mcp http server listening", "addr", l.Addr().String(), "auth", auth)
	return RunHTTP(ctx, l, Protect(opts.Logger, token, mux))
}

// RunHTTP serves h on l until ctx is done.
func RunHTTP(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Protect wraps h with request logging and, when token is set, the shared
// secret check.
func Protect(logger *slog.Logger, token string, h http.Handler) http.Handler {
	if token != "" {
		h = requireToken(logger, token, h)
	}
	return logRequests(logger, h)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController find the real Flusher; without it the
// SDK's SSE flushes fail silently and streamed responses never arrive.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("http request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"remote", r.RemoteAddr, "session", r.Header.Get("Mcp-Session-Id"),
			"duration", time.Since(started).Round(time.Millisecond).String())
	})
}

// cookieName carries the token for browsers, which cannot set headers.
const cookieName = "kb_token"

// requireToken accepts `Authorization: Bearer <token>` (MCP clients), or a
// kb_token cookie (browsers). A browser gets the cookie by visiting any page
// once with ?token=<token>, which redirects to the same URL without it so the
// secret does not stay in the address bar or the history.
func requireToken(logger *slog.Logger, token string, next http.Handler) http.Handler {
	want := []byte(token)
	ok := func(got string) bool { return got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1 }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found && ok(b) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil && ok(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if q := r.URL.Query().Get("token"); q != "" && ok(q) && r.Method == http.MethodGet {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			u := *r.URL
			v := u.Query()
			v.Del("token")
			u.RawQuery = v.Encode()
			http.Redirect(w, r, (&url.URL{Path: u.Path, RawQuery: u.RawQuery}).String(), http.StatusSeeOther)
			return
		}
		logger.Warn("auth rejected", "remote", r.RemoteAddr, "path", r.URL.Path)
		w.Header().Set("WWW-Authenticate", `Bearer realm="kb"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func isLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
