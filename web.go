package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	webResults = 8
	webPage    = 10000 // chars of page text that fetch_url returns per call
	webBody    = 4 << 20
	webTimeout = 20 * time.Second
	webAgent   = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
)

var (
	dropBlocks = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer|template)\b.*?</(script|style|noscript|svg|head|nav|footer|template)>`)
	comments   = regexp.MustCompile(`(?s)<!--.*?-->`)
	preBlock   = regexp.MustCompile(`(?is)<pre\b[^>]*>(.*?)</pre>`)
	breakTags  = regexp.MustCompile(`(?i)<(br|/p|/div|/h[1-6]|/tr|/section|/article|/table|/ul|/ol|/blockquote|/dt|/dd)\b[^>]*>`)
	headTag    = regexp.MustCompile(`(?i)<h([1-6])\b[^>]*>`)
	itemTag    = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	anyTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	spaces     = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankRuns  = regexp.MustCompile(`\n{3,}`)
)

func webSchemas() []map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	return []map[string]any{
		{"type": "function", "function": map[string]any{
			"name":        "web_search",
			"description": "Search the web. Returns titles, URLs and snippets. Use it for docs, API references, versions and error messages; then fetch_url the best page.",
			"parameters": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{
				"query": str("search words, for example: go context WithTimeout example")}},
		}},
		{"type": "function", "function": map[string]any{
			"name":        "fetch_url",
			"description": "Fetch a web page as plain text, 10000 chars at a time. Use offset to read further.",
			"parameters": map[string]any{"type": "object", "required": []string{"url"}, "properties": map[string]any{
				"url":    str("http or https URL"),
				"offset": map[string]any{"type": "integer", "description": "first char, from the [chars a-b of n] line of the last call"}}},
		}},
	}
}

func webGet(ctx context.Context, req *http.Request) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, webTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", webAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, webBody))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), resp.Header.Get("Content-Type"), nil
}

// webSearch asks a SearXNG instance (config search_url) through its JSON API.
func webSearch(ctx context.Context, base, q string) (string, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "error: web_search needs a query", false
	}
	if base == "" {
		return "error: web_search has no search_url in ~/.qh/config.json (a SearXNG instance)", false
	}
	req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/search?"+url.Values{"q": {q}, "format": {"json"}}.Encode(), nil)
	if err != nil {
		return "error: search_url: " + err.Error(), false
	}
	body, _, err := webGet(ctx, req)
	if err != nil {
		return "error: search failed: " + err.Error(), false
	}
	var res struct {
		Results []struct{ Title, URL, Content string }
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return "error: search_url did not answer JSON; SearXNG needs json in search.formats", false
	}
	if len(res.Results) == 0 {
		return "no results for " + q, true
	}
	var b strings.Builder
	for i, r := range res.Results[:min(len(res.Results), webResults)] {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, plainText(r.Title), r.URL)
		if c := plainText(r.Content); c != "" {
			fmt.Fprintf(&b, "   %s\n", c)
		}
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func plainText(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(strings.ReplaceAll(html.UnescapeString(anyTag.ReplaceAllString(s, "")), "\n", " "), " "))
}

// pageText turns HTML into readable text: headings get #, list items -,
// pre blocks keep their lines.
func pageText(h string) string {
	h = comments.ReplaceAllString(h, "")
	h = dropBlocks.ReplaceAllString(h, "")
	var pres []string
	h = preBlock.ReplaceAllStringFunc(h, func(m string) string {
		inner := preBlock.FindStringSubmatch(m)[1]
		pres = append(pres, html.UnescapeString(anyTag.ReplaceAllString(inner, "")))
		return fmt.Sprintf("\n\x00%d\x00\n", len(pres)-1)
	})
	h = headTag.ReplaceAllStringFunc(h, func(m string) string {
		return "\n\n" + strings.Repeat("#", int(headTag.FindStringSubmatch(m)[1][0]-'0')) + " "
	})
	h = itemTag.ReplaceAllString(h, "\n- ")
	h = breakTags.ReplaceAllString(h, "\n")
	h = html.UnescapeString(anyTag.ReplaceAllString(h, ""))
	lines := strings.Split(h, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaces.ReplaceAllString(l, " "))
	}
	h = blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	for i, p := range pres {
		h = strings.Replace(h, fmt.Sprintf("\x00%d\x00", i), "```\n"+strings.Trim(p, "\n")+"\n```", 1)
	}
	return strings.TrimSpace(h)
}

func fetchURL(ctx context.Context, raw string, off int) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "error: fetch_url needs an http or https URL", false
	}
	req, _ := http.NewRequest("GET", u.String(), nil)
	body, ct, err := webGet(ctx, req)
	if err != nil {
		return "error: fetch failed: " + err.Error(), false
	}
	text := body
	if strings.Contains(ct, "html") || (ct == "" && strings.Contains(body[:min(len(body), 512)], "<")) {
		text = pageText(body)
	} else if !strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
		return "error: " + u.String() + " is " + ct + ", not text", false
	}
	r := []rune(text)
	off = max(0, min(off, len(r)))
	end := min(len(r), off+webPage)
	out := string(r[off:end])
	if off > 0 || end < len(r) {
		out += fmt.Sprintf("\n[chars %d-%d of %d", off, end, len(r))
		if end < len(r) {
			out += fmt.Sprintf("; offset %d reads on", end)
		}
		out += "]"
	}
	return out, true
}
