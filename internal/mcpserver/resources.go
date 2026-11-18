package mcpserver

import (
	"context"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xen0bit/kb/internal/store"
)

// registerResources exposes pages and files as MCP resources, for clients
// that attach resources to a conversation rather than calling tools.
func (t *tools) registerResources(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         "kb://guide",
		Name:        "guide",
		Title:       "How to write kb pages",
		Description: "Link syntax, frontmatter, redirects and editing conventions.",
		MIMEType:    "text/markdown",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: instructions}}}, nil
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "kb://page/{title}",
		Name:        "page",
		Title:       "kb page",
		Description: "A page's current markdown. Title with underscores for spaces.",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		title := strings.TrimPrefix(req.Params.URI, "kb://page/")
		if u, err := url.PathUnescape(title); err == nil {
			title = u
		}
		p, err := t.st.GetPage(ctx, title, store.GetOptions{Body: true, FollowRedirect: true})
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: p.Body}}}, nil
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "kb://file/{name}",
		Name:        "file",
		Title:       "kb file",
		Description: "An uploaded file's bytes.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		name := strings.TrimPrefix(req.Params.URI, "kb://file/")
		if u, err := url.PathUnescape(name); err == nil {
			name = u
		}
		f, data, err := t.st.GetFile(ctx, name, true, 0)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		rc := &mcp.ResourceContents{URI: req.Params.URI, MIMEType: f.MIME}
		if isTextMIME(f.MIME) {
			rc.Text = string(data)
		} else {
			rc.Blob = data
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{rc}}, nil
	})
}
