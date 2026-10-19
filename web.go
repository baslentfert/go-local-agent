package main

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

func get(u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	return string(b), err
}

var (
	reResult  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?class="result__snippet"[^>]*>(.*?)</a>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reDrop    = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer)[^>]*>.*?</(script|style|noscript|svg|head|nav|footer)>`)
	reBlock   = regexp.MustCompile(`(?i)</?(p|div|br|li|h[1-6]|tr|section|article)[^>]*>`)
	reSpaces  = regexp.MustCompile(`[ \t\r]+`)
	reNewline = regexp.MustCompile(`\n\s*\n+`)
)

func stripTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(s, "")))
}

// webSearch gebruikt de HTML-versie van DuckDuckGo (geen API-sleutel nodig).
func webSearch(q string) string {
	page, err := get("https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q))
	if err != nil {
		return "fout: " + err.Error()
	}
	var b strings.Builder
	n, ads := 0, 0
	for _, m := range reResult.FindAllStringSubmatch(page, -1) {
		link := html.UnescapeString(m[1])
		// DuckDuckGo verpakt links als //duckduckgo.com/l/?uddg=<echte url>
		if pu, err := url.Parse(link); err == nil && pu.Query().Get("uddg") != "" {
			link = pu.Query().Get("uddg")
		}
		// Advertenties (duckduckgo.com/y.js?ad_domain=…) overslaan: een klein model leest
		// "Best Futures Prop Firm" in een advertentie anders als onafhankelijk zoekresultaat.
		// Pas ná het uitpakken controleren: de advertentielink zit gecodeerd in uddg=.
		if strings.Contains(link, "/y.js?") || strings.Contains(link, "ad_domain=") {
			ads++
			continue
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", n, stripTags(m[2]), link, stripTags(m[3]))
		if n == 8 {
			break
		}
	}
	if n == 0 {
		return "geen resultaten"
	}
	b.WriteString("\n(Dit zijn alleen titels en korte fragmenten van zoekresultaten, geen gecontroleerde feiten. Open een bron met fetch_url om iets na te gaan.)")
	if ads > 0 {
		fmt.Fprintf(&b, "\n(%d advertentie(s) weggelaten.)", ads)
	}
	return b.String()
}

func fetchURL(u string) string {
	page, err := get(u)
	if err != nil {
		return "fout: " + err.Error()
	}
	page = reDrop.ReplaceAllString(page, "")
	page = reBlock.ReplaceAllString(page, "\n")
	text := html.UnescapeString(reTag.ReplaceAllString(page, ""))
	text = reSpaces.ReplaceAllString(text, " ")
	text = reNewline.ReplaceAllString(text, "\n\n")
	return truncate(strings.TrimSpace(text))
}
