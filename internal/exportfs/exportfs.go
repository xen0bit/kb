// Package exportfs moves a kb to and from a directory of plain files:
//
//	<dir>/pages/<Title>.md     current text of every live page, frontmatter included
//	<dir>/files/<Name>         every live file's bytes
//	<dir>/manifest.json        exact titles, revs and file metadata
//
// File names are titles in URL form (spaces as underscores, unsafe bytes
// percent-encoded), so any title survives a round trip; manifest.json keeps
// the exact title anyway. By default pages are written verbatim, kb: links
// and all, which is what Import reads back. Portable rewrites kb: links into
// relative paths (Title.md, ../files/Name) so the folder reads well in an
// editor or on a git forge; that export is for reading, not re-importing.
package exportfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/xen0bit/kb/internal/store"
	"github.com/xen0bit/kb/internal/wikitext"
)

// Manifest describes an export.
type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Pages         []ManifestPage `json:"pages"`
	Files         []ManifestFile `json:"files"`
}

// ManifestPage is one exported page.
type ManifestPage struct {
	Title string `json:"title"`
	Path  string `json:"path"`
	Rev   int64  `json:"rev"`
}

// ManifestFile is one exported file.
type ManifestFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	MIME        string `json:"mime"`
	SHA256      string `json:"sha256"`
	Description string `json:"description,omitempty"`
}

// Options controls Export.
type Options struct {
	Portable bool
	Progress func(kind, name string)
}

// fileName is a title as a file name. URLForm already escapes '/', '%' and
// the characters shells and editors trip over.
func fileName(title string) string { return wikitext.URLForm(title) }

// Export writes the kb to dir, which must be empty or not exist.
func Export(ctx context.Context, st *store.Store, dir string, opt Options) (*Manifest, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty", dir)
	}
	for _, d := range []string{"pages", "files"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	v, _ := st.Version(ctx)
	m := &Manifest{SchemaVersion: v}
	for off := 0; ; {
		list, total, err := st.ListPages(ctx, store.PageFilter{Redirects: true}, "title", nil, 500, off)
		if err != nil {
			return nil, err
		}
		for _, p := range list {
			rp, err := st.GetPage(ctx, p.Title, store.GetOptions{Body: true})
			if err != nil {
				return nil, err
			}
			body := rp.Body
			if opt.Portable {
				body = portable(body)
			}
			rel := filepath.Join("pages", fileName(rp.Title)+".md")
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
				return nil, err
			}
			m.Pages = append(m.Pages, ManifestPage{Title: rp.Title, Path: filepath.ToSlash(rel), Rev: rp.Rev})
			if opt.Progress != nil {
				opt.Progress("page", rp.Title)
			}
		}
		off += len(list)
		if off >= total || len(list) == 0 {
			break
		}
	}
	for off := 0; ; {
		files, total, err := st.ListFiles(ctx, store.FileFilter{Limit: 200, Offset: off})
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			_, data, err := st.GetFile(ctx, f.Name, true, 0)
			if err != nil {
				return nil, err
			}
			rel := filepath.Join("files", fileName(f.Name))
			if err := os.WriteFile(filepath.Join(dir, rel), data, 0o644); err != nil {
				return nil, err
			}
			m.Files = append(m.Files, ManifestFile{Name: f.Name, Path: filepath.ToSlash(rel), MIME: f.MIME, SHA256: f.SHA256, Description: f.Description})
			if opt.Progress != nil {
				opt.Progress("file", f.Name)
			}
		}
		off += len(files)
		if off >= total || len(files) == 0 {
			break
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644)
}

var kbDest = regexp.MustCompile(`\]\(\s*(<?)((?i:kb-file|kb)):([^)>\s#]*|[^)>#]*)(#[^)>\s]*)?(>?)`)

// portable rewrites kb: and kb-file: destinations into relative paths.
func portable(body string) string {
	return kbDest.ReplaceAllStringFunc(body, func(m string) string {
		sub := kbDest.FindStringSubmatch(m)
		kind, target, frag := strings.ToLower(sub[2]), sub[3], sub[4]
		if t, err := url.PathUnescape(target); err == nil {
			target = t
		}
		if kind == "kb-file" {
			n, err := wikitext.NormalizeFileName(target)
			if err != nil {
				return m
			}
			return "](../files/" + fileName(n)
		}
		n, err := wikitext.NormalizeTitle(target)
		if err != nil {
			return m
		}
		if frag != "" {
			frag = "#" + wikitext.Anchor(frag[1:])
		}
		return "](" + fileName(n) + ".md" + frag
	})
}

// ImportResult counts what Import did.
type ImportResult struct {
	Created, Updated, Unchanged int
	FilesCreated, FilesUpdated  int
	FilesUnchanged              int
	Errors                      []string
}

// Import reads a directory written by Export (or any folder of .md files
// under pages/ or at the top level) into the kb. Existing pages are
// replaced; identical ones are left alone, so importing twice is a no-op.
func Import(ctx context.Context, st *store.Store, dir, author string, progress func(kind, name string)) (*ImportResult, error) {
	res := &ImportResult{}
	titles := map[string]string{} // slash path → exact title
	fileMeta := map[string]ManifestFile{}
	if b, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("manifest.json: %w", err)
		}
		for _, p := range m.Pages {
			titles[p.Path] = p.Title
		}
		for _, f := range m.Files {
			fileMeta[f.Path] = f
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	pagesDir := filepath.Join(dir, "pages")
	if _, err := os.Stat(pagesDir); err != nil {
		pagesDir = dir
	}
	// Files first, so pages that embed them are not reported as missing.
	filesDir := filepath.Join(dir, "files")
	if entries, err := os.ReadDir(filesDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := "files/" + e.Name()
			meta := fileMeta[rel]
			name := meta.Name
			if name == "" {
				name = unescapeName(e.Name())
			}
			data, err := os.ReadFile(filepath.Join(filesDir, e.Name()))
			if err != nil {
				return nil, err
			}
			var base int64
			if f, _, err := st.GetFile(ctx, name, false, 0); err == nil {
				base = f.Rev
			}
			r, err := st.PutFile(ctx, store.PutFileRequest{Name: name, MIME: meta.MIME, Data: data, Description: meta.Description,
				Comment: "import", Author: author, BaseRev: base, MaxBytes: 1 << 40})
			switch {
			case err != nil:
				res.Errors = append(res.Errors, fmt.Sprintf("file %s: %v", name, err))
			case r.Unchanged:
				res.FilesUnchanged++
			case r.Created:
				res.FilesCreated++
			default:
				res.FilesUpdated++
			}
			if progress != nil {
				progress("file", name)
			}
		}
	}
	var mdFiles []string
	err := filepath.WalkDir(pagesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != pagesDir && (d.Name() == "files" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			mdFiles = append(mdFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(mdFiles)
	for _, path := range mdFiles {
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		title := titles[rel]
		if title == "" {
			title = unescapeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		r, err := st.PutPage(ctx, store.PutRequest{Title: title, Body: string(body), Comment: "import", Author: author, Mode: store.ModeUpsert})
		switch {
		case err != nil:
			res.Errors = append(res.Errors, fmt.Sprintf("page %s: %v", title, err))
		case r.Unchanged:
			res.Unchanged++
		case r.Created:
			res.Created++
		default:
			res.Updated++
		}
		if progress != nil {
			progress("page", title)
		}
	}
	return res, nil
}

func unescapeName(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		s = u
	}
	return strings.ReplaceAll(s, "_", " ")
}
