package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xen0bit/kb/internal/store"
)

// toJSON renders a result as compact JSON, with every timestamp field
// (unix ms in the store) rewritten as RFC 3339 UTC, which a model reads
// without arithmetic.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	var generic any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&generic); err != nil {
		return string(b)
	}
	out, err := json.Marshal(humanTimes(generic, ""))
	if err != nil {
		return string(b)
	}
	return string(out)
}

func isTimeKey(k string) bool {
	return k == "at" || strings.HasSuffix(k, "_at") || k == "lease_until"
}

func humanTimes(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = humanTimes(e, k)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = humanTimes(e, "")
		}
		return t
	case json.Number:
		if isTimeKey(key) {
			if ms, err := t.Int64(); err == nil && ms > 0 {
				return time.UnixMilli(ms).UTC().Format(time.RFC3339)
			}
		}
		return t
	}
	return v
}

// parseTime accepts what an agent is likely to write for a point in time:
// RFC 3339, a date, unix milliseconds, or a duration back from now ("24h",
// "-24h", "7d", "30m").
func parseTime(s string, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse("2006-01-02T15:04", s); err == nil {
		return t.UnixMilli(), nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 1e11 { // seconds, not milliseconds
			n *= 1000
		}
		return n, nil
	}
	d := strings.TrimPrefix(s, "-")
	mult := time.Duration(1)
	if days, ok := strings.CutSuffix(d, "d"); ok {
		d, mult = days+"h", 24
	}
	if dur, err := time.ParseDuration(d); err == nil {
		return now.Add(-dur * mult).UnixMilli(), nil
	}
	return 0, fmt.Errorf("%w: cannot read %q as a time: use RFC 3339 (2026-01-02T15:04:05Z), a date (2026-01-02), or a duration back from now (24h, 7d)", store.ErrInvalid, s)
}

// decodeBase64 accepts standard or URL-safe base64, padded or not, with
// whitespace, and with or without a data: URL prefix.
func decodeBase64(s string) ([]byte, string, error) {
	mt := ""
	if rest, ok := strings.CutPrefix(s, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		if !found {
			return nil, "", errors.New("data URL has no comma")
		}
		mt, _, _ = strings.Cut(meta, ";")
		s = data
	}
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		b, err := base64.RawURLEncoding.DecodeString(s)
		return b, mt, err
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	return b, mt, err
}

// page describes a paginated result.
type page struct {
	Total      int `json:"total"`
	Offset     int `json:"offset"`
	Returned   int `json:"returned"`
	NextOffset int `json:"next_offset,omitempty"`
}

func paging(total, offset, returned int) page {
	p := page{Total: total, Offset: offset, Returned: returned}
	if offset+returned < total {
		p.NextOffset = offset + returned
	}
	return p
}

// truncateBytes cuts text at a line boundary at or below max bytes. It
// returns the kept text and the number of lines kept.
func truncateBytes(text string, maxBytes int) (string, int, bool) {
	if len(text) <= maxBytes {
		return text, strings.Count(text, "\n"), false
	}
	cut := strings.LastIndexByte(text[:maxBytes], '\n')
	if cut < 0 {
		// One enormous line: cut on a rune boundary.
		cut = maxBytes
		for cut > 0 && (text[cut]&0xC0) == 0x80 {
			cut--
		}
		return text[:cut], 0, true
	}
	return text[:cut+1], strings.Count(text[:cut+1], "\n"), true
}
