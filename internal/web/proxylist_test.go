package web

import (
	"strings"
	"testing"
)

var (
	flagUS    = string(rune(0x1F1FA)) + string(rune(0x1F1F8))
	flagGB    = string(rune(0x1F1EC)) + string(rune(0x1F1E7))
	zeroWidth = string(rune(0x200B))
)

type parsedLine struct {
	line  int
	proxy string // URL, or "" when the entry is an error
}

func summarize(t *testing.T, text string) []parsedLine {
	t.Helper()
	got, err := parseProxyList(text)
	if err != nil {
		t.Fatalf("parseProxyList: %v", err)
	}
	out := make([]parsedLine, len(got))
	for i, l := range got {
		out[i].line = l.Line
		if l.Err == nil {
			out[i].proxy = l.Proxy.String()
		}
	}
	return out
}

func assertParsed(t *testing.T, got, want []parsedLine) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d entries %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The Webshare panel table copied one cell per line, with flags, invisible characters, status,
// latency, "Copy" and blank separator lines around the data.
func TestParseProxyList_ProviderTableOneCellPerLine(t *testing.T) {
	text := strings.Join([]string{
		flagUS + " United States", // 1
		"New York",                // 2
		"1.2.3.4" + zeroWidth,     // 3
		"8080",                    // 4
		"userA",                   // 5
		"",                        // 6
		"passA",                   // 7
		"Working",                 // 8
		"231 ms",                  // 9
		"Copy",                    // 10
		flagGB + " United Kingdom",
		"London",
		"5.6.7.8", // 13
		"3128",
		"userB",
		"p:ss:B", // colon inside the password
		"Not Working",
		"5 minutes ago",
	}, "\n")
	assertParsed(t, summarize(t, text), []parsedLine{
		{3, "http://userA:passA@1.2.3.4:8080"},
		{13, "http://userB:p%3Ass%3AB@5.6.7.8:3128"},
	})
}

// The same table copied with one row per line and tab-separated columns.
func TestParseProxyList_ProviderTableTabSeparated(t *testing.T) {
	text := "Country\tCity\tProxy Address\tPort\tUsername\tPassword\tStatus\n" +
		flagUS + " United States\tNew York\t1.2.3.4\t8080\tuserA\tpassA\tWorking\n" +
		flagGB + " United Kingdom\tLondon\t5.6.7.8\t3128\tuserB\tpassB\tWorking\n"
	assertParsed(t, summarize(t, text), []parsedLine{
		{2, "http://userA:passA@1.2.3.4:8080"},
		{3, "http://userB:passB@5.6.7.8:3128"},
	})
}

func TestParseProxyList_ProviderTableEdgeCases(t *testing.T) {
	text := strings.Join([]string{
		"1.2.3.4", // 1: no credentials before the next row starts
		"8080",
		"Working",
		"5.6.7.8", // 4: complete
		"3128",
		"userB",
		"passB",
		"http://alice:pw@proxy.example.com:3128", // 8: a full proxy among the table is kept
		"12:30",                                  // looks like host:port, but is just a cell here
	}, "\n")
	got := summarize(t, text)
	assertParsed(t, got, []parsedLine{
		{1, ""},
		{4, "http://userB:passB@5.6.7.8:3128"},
		{8, "http://alice:pw@proxy.example.com:3128"},
	})

	entries, _ := parseProxyList(text)
	if msg := entries[0].Err.Error(); !strings.Contains(msg, "no username and password") {
		t.Errorf("missing-credentials error = %q", msg)
	}
}

// Without an IP cell followed by a port cell, the text is one proxy per line, as before.
func TestParseProxyList_LineFormatUnchanged(t *testing.T) {
	text := strings.Join([]string{
		"# comment",
		"1.2.3.4:8080:userA:passA",
		"socks5://5.6.7.8:1080",
		"",
		"not a proxy",
	}, "\n")
	assertParsed(t, summarize(t, text), []parsedLine{
		{2, "http://userA:passA@1.2.3.4:8080"},
		{3, "socks5://5.6.7.8:1080"},
		{5, ""},
	})
}

// The import re-sends the text and selected line numbers, so parsing must be deterministic.
func TestParseProxyList_StableLineNumbers(t *testing.T) {
	text := flagUS + " United States\nNew York\n1.2.3.4\n8080\nuserA\npassA\nWorking\n"
	a, b := summarize(t, text), summarize(t, text)
	assertParsed(t, b, a)
}

func TestParseProxyList_ErrorsNeverEchoCredentials(t *testing.T) {
	entries, err := parseProxyList("1.2.3.4\n8080\ns3cretUser\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Err != nil && strings.Contains(e.Err.Error(), "s3cretUser") {
			t.Errorf("error echoes a credential cell: %v", e.Err)
		}
	}
}
