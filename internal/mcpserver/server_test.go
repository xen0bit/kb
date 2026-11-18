package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xen0bit/kb/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kb.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func connect(t *testing.T, st *store.Store) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := NewServer(st, Options{Version: "test", Logger: slog.New(slog.DiscardHandler)})
	ct, stt := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call invokes a tool and returns its text, failing the test on a tool
// error unless wantErr is set.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, wantErr bool) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if res.IsError != wantErr {
		t.Fatalf("%s: IsError=%v, want %v: %s", name, res.IsError, wantErr, out)
	}
	return out
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	line, _, _ := strings.Cut(s, "\n\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func TestAgentSession(t *testing.T) {
	cs := connect(t, newStore(t))

	tl, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Tools) < 25 {
		t.Errorf("only %d tools", len(tl.Tools))
	}

	ov := call(t, cs, "kb_overview", nil, false)
	if !strings.Contains(ov, `"hint"`) || !strings.Contains(ov, "kb:Page_Title") {
		t.Errorf("overview of empty kb: %s", ov)
	}

	call(t, cs, "task_create", map[string]any{"tasks": []any{
		map[string]any{"title": "Write the solar system", "priority": 5},
	}}, false)
	claim := decode(t, call(t, cs, "task_claim_next", nil, false))
	task := claim["task"].(map[string]any)
	if task["assignee"] != "test-agent" || task["status"] != "doing" {
		t.Errorf("claim %v", task)
	}

	created := decode(t, call(t, cs, "create_page", map[string]any{
		"title": "Solar System",
		"body":  "---\ncategories: [Astronomy]\n---\nThe [Sun](kb:Sun) and its planets, such as [Earth](kb:Earth).\n\n# Planets\n\n## Inner\n\n[Mercury](kb:Mercury).\n\n## Outer\n\n[Jupiter](kb:Jupiter).\n\n![diagram](kb-file:Solar_system.png)\n",
	}, false))
	if len(created["red_links"].([]any)) != 4 || len(created["missing_files"].([]any)) != 1 {
		t.Errorf("create %v", created)
	}

	bulk := decode(t, call(t, cs, "bulk_write", map[string]any{"pages": []any{
		map[string]any{"title": "Sun", "body": "A star at the centre of the [Solar System](kb:Solar_System)."},
		map[string]any{"title": "Earth", "body": "---\nstatus: stub\n---\nThird planet."},
		map[string]any{"title": "Solar System", "body": "dup"},
	}}, false))
	if bulk["written"].(float64) != 2 || bulk["failed"].(float64) != 1 {
		t.Errorf("bulk %v", bulk)
	}

	wanted := decode(t, call(t, cs, "special_pages", map[string]any{"kind": "wanted"}, false))
	if n := len(wanted["entries"].([]any)); n != 2 {
		t.Errorf("wanted %v", wanted)
	}

	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	up := decode(t, call(t, cs, "upload_file", map[string]any{"name": "Solar_system.png", "data_base64": png, "description": "diagram"}, false))
	if up["markdown"] != "![Solar system.png](kb-file:Solar_system.png)" {
		t.Errorf("upload %v", up)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_file", Arguments: map[string]any{"name": "solar system.png", "content": true}})
	if err != nil || res.IsError || len(res.Content) != 2 {
		t.Fatalf("get_file %v %v", res, err)
	}
	if img, ok := res.Content[1].(*mcp.ImageContent); !ok || img.MIMEType != "image/png" {
		t.Errorf("get_file content %#v", res.Content[1])
	}

	outline := call(t, cs, "read_page", map[string]any{"title": "solar_system", "outline": true}, false)
	if !strings.Contains(outline, `"path":"Planets/Outer"`) {
		t.Errorf("outline %s", outline)
	}
	sec := call(t, cs, "read_page", map[string]any{"title": "Solar System", "section": "Inner"}, false)
	head := decode(t, sec)
	if !strings.HasSuffix(sec, "## Inner\n\n[Mercury](kb:Mercury).\n\n") || head["backlinks"].(float64) != 1 {
		t.Errorf("section read %s", sec)
	}
	if !strings.Contains(head["updated_at"].(string), "T") {
		t.Errorf("times not humanized: %v", head["updated_at"])
	}
	rev := int64(head["rev"].(float64))

	// Someone else edits; our stale write conflicts, our str_replace still applies.
	call(t, cs, "edit_page", map[string]any{"title": "Solar System", "append": "See also [Moon](kb:Moon).", "author": "other"}, false)
	conflict := call(t, cs, "write_page", map[string]any{"title": "Solar System", "body": "x", "base_rev": rev}, true)
	if !strings.Contains(conflict, "edit conflict") || !strings.Contains(conflict, "other") {
		t.Errorf("conflict %s", conflict)
	}
	call(t, cs, "edit_page", map[string]any{"title": "Solar System", "edits": []any{map[string]any{"old_text": "[Jupiter](kb:Jupiter).", "new_text": "[Jupiter](kb:Jupiter) and [Saturn](kb:Saturn)."}}}, false)
	diff := call(t, cs, "diff_revisions", map[string]any{"title": "Solar System"}, false)
	if !strings.Contains(diff, "+[Jupiter](kb:Jupiter) and [Saturn](kb:Saturn).") {
		t.Errorf("diff %s", diff)
	}

	search := decode(t, call(t, cs, "search", map[string]any{"query": "planet", "sort": "updated"}, false))
	if search["page"].(map[string]any)["total"].(float64) < 2 {
		t.Errorf("search %v", search)
	}
	miss := call(t, cs, "read_page", map[string]any{"title": "Solar Sistem"}, true)
	if !strings.Contains(miss, "did you mean: Solar System") {
		t.Errorf("did you mean: %s", miss)
	}

	moved := decode(t, call(t, cs, "move_page", map[string]any{"from": "Sun", "to": "Sun (star)"}, false))
	if moved["links_rewritten"].(float64) != 1 || moved["redirect_left"] != true {
		t.Errorf("move %v", moved)
	}
	viaRedirect := decode(t, call(t, cs, "read_page", map[string]any{"title": "Sun"}, false))
	if viaRedirect["redirected_from"] != "Sun" || viaRedirect["title"] != "Sun (star)" {
		t.Errorf("redirect %v", viaRedirect)
	}

	call(t, cs, "note_write", map[string]any{"topic": "progress", "body": "Solar System drafted; next: planets."}, false)
	call(t, cs, "task_update", map[string]any{"id": task["id"], "status": "done", "append": "drafted"}, false)

	changes := decode(t, call(t, cs, "recent_changes", map[string]any{"after_id": 1, "kind": "page"}, false))
	if len(changes["changes"].([]any)) < 5 {
		t.Errorf("changes %v", changes)
	}
	ov = call(t, cs, "kb_overview", nil, false)
	if !strings.Contains(ov, "Solar System drafted") || strings.Contains(ov, `"hint"`) {
		t.Errorf("overview %s", ov)
	}

	fb := call(t, cs, "kb_feedback", map[string]any{"kind": "friction", "tool": "read_page", "message": "wanted X"}, false)
	if !strings.Contains(fb, "recorded") {
		t.Errorf("feedback %s", fb)
	}
	call(t, cs, "kb_feedback", map[string]any{"kind": "nope", "message": "x"}, true)

	rr, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "kb://page/Sun_%28star%29"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "A star") {
		t.Errorf("resource %v %v", rr, err)
	}
}

func TestReadTruncation(t *testing.T) {
	st := newStore(t)
	var b strings.Builder
	for i := range 2000 {
		b.WriteString("Line number ")
		b.WriteString(strings.Repeat("x", i%50))
		b.WriteString("\n")
	}
	if _, err := st.PutPage(context.Background(), store.PutRequest{Title: "Big", Body: b.String(), Mode: store.ModeCreate}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, st)
	out := call(t, cs, "read_page", map[string]any{"title": "Big", "max_bytes": 1000}, false)
	h := decode(t, out)
	cont, _ := h["continue"].(string)
	if !strings.Contains(cont, "start_line=") || len(out) > 2000 {
		t.Errorf("truncation header %v (%d bytes)", h, len(out))
	}
	next := decode(t, call(t, cs, "read_page", map[string]any{"title": "Big", "start_line": 30, "end_line": 31}, false))
	if next["showing"] != "lines 30-31 of 2000" {
		t.Errorf("range %v", next)
	}
}

func TestHTTPAuth(t *testing.T) {
	st := newStore(t)
	logger := slog.New(slog.DiscardHandler)
	h := Protect(logger, "s3cret", Handler(st, Options{Logger: logger}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d", resp.StatusCode)
	}

	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearer{"s3cret"}}}
	client := mcp.NewClient(&mcp.Implementation{Name: "http-agent"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call(t, cs, "kb_overview", nil, false)

	// The cookie path for browsers: ?token= sets it and redirects.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.Get(srv.URL + "/wiki/x?token=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 || strings.Contains(resp.Header.Get("Location"), "token") {
		t.Errorf("cookie login: %d %v %q", resp.StatusCode, resp.Cookies(), resp.Header.Get("Location"))
	}

	if _, err := Listen("0.0.0.0:0", "", "MCP"); err == nil || !strings.Contains(err.Error(), TokenEnv) {
		t.Errorf("non-loopback without token: %v", err)
	}
	l, err := Listen("127.0.0.1:0", "", "MCP")
	if err != nil {
		t.Errorf("loopback: %v", err)
	} else {
		l.Close()
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
