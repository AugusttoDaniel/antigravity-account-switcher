package web

// Pasted proxy lists come in two shapes:
//
//   - one proxy per line: a proxy URL, host:port:user:pass or host:port;
//   - a provider table, as copying the proxy table from a panel such as Webshare's yields: one cell
//     per line (or several cells per line, tab-separated), with the IP, then the port, then the
//     username and password, among country, city, status, latency and "Copy" cells.
//
// The table is read the way the webshare-to-txt converter did: find an IPv4 cell, take the port in
// the next cell, then the next two cells that are not noise as username and password.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/AugusttoDaniel/antigravity-account-switcher/internal/egress"
)

var (
	ipv4Cell  = regexp.MustCompile(`^(?:\d{1,3}\.){3}\d{1,3}$`)
	portCell  = regexp.MustCompile(`^\d{2,5}$`)
	noiseCell = regexp.MustCompile(`(?i)^(working|not working|offline|online|copy|copied|[\d.]+\s*(ms|s)|.*\bago\b.*|\d+\s+(minute|hour|day|second)s?)$`)
)

// listCell is one non-empty cell of the pasted text and the 1-based line it came from.
type listCell struct {
	line  int
	value string
}

// cleanCell drops what panel copies carry around the data (flag emoji, zero-width characters, a
// BOM) and trims whitespace.
func cleanCell(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators, i.e. flag emoji
			return -1
		case r >= 0x200B && r <= 0x200D, r == 0x2060, r == 0xFEFF: // zero-width characters, BOM
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func isIPv4Cell(s string) bool {
	if !ipv4Cell.MatchString(s) {
		return false
	}
	for _, octet := range strings.Split(s, ".") {
		if n, _ := strconv.Atoi(octet); n > 255 {
			return false
		}
	}
	return true
}

// isFullProxyCell reports whether one cell is a complete proxy on its own: a URL or the
// host:port:user:pass form. Bare host:port is left out because table cells such as "12:30" would
// read as one.
func isFullProxyCell(s string) bool {
	return strings.Contains(s, "://") || strings.Count(s, ":") >= 3
}

// parseProxyList turns pasted text into proxies, each tagged with the line it starts on so results
// can point back at the input. The same text always yields the same lines, which the import relies
// on: it re-sends the text and the selected line numbers.
func parseProxyList(text string) ([]proxyLine, error) {
	var lines, cells []listCell
	for i, raw := range strings.Split(text, "\n") {
		s := cleanCell(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		lines = append(lines, listCell{line: i + 1, value: s})
		for _, field := range strings.Split(s, "\t") {
			if field = cleanCell(field); field != "" {
				cells = append(cells, listCell{line: i + 1, value: field})
			}
		}
	}

	var out []proxyLine
	if isProviderTable(cells) {
		out = parseProviderTable(cells)
	} else {
		for _, l := range lines {
			u, err := egress.ParseProxyLine(l.value)
			out = append(out, proxyLine{Line: l.line, Proxy: u, Err: err})
		}
	}
	if len(out) > maxProxyLines {
		return nil, fmt.Errorf("too many proxies: at most %d per request", maxProxyLines)
	}
	return out, nil
}

// isProviderTable reports whether the cells hold at least one IP cell directly followed by a port
// cell, the signature of a copied provider table.
func isProviderTable(cells []listCell) bool {
	for i := 0; i+1 < len(cells); i++ {
		if isIPv4Cell(cells[i].value) && portCell.MatchString(cells[i+1].value) {
			return true
		}
	}
	return false
}

func parseProviderTable(cells []listCell) []proxyLine {
	var out []proxyLine
	for i := 0; i < len(cells); i++ {
		c := cells[i]
		if !isIPv4Cell(c.value) {
			// A complete proxy may still sit among the table; everything else (country, city,
			// status, "Copy"...) is not an error, just not a proxy.
			if isFullProxyCell(c.value) {
				u, err := egress.ParseProxyLine(c.value)
				out = append(out, proxyLine{Line: c.line, Proxy: u, Err: err})
			}
			continue
		}
		if i+1 >= len(cells) || !portCell.MatchString(cells[i+1].value) {
			out = append(out, proxyLine{Line: c.line, Err: fmt.Errorf("%w: IP %s has no port after it", egress.ErrInvalidProxy, c.value)})
			continue
		}
		port := cells[i+1].value

		var creds []string
		j := i + 2
		for ; j < len(cells) && len(creds) < 2; j++ {
			v := cells[j].value
			if isIPv4Cell(v) {
				break // the next row starts before this one had credentials
			}
			if !noiseCell.MatchString(v) {
				creds = append(creds, v)
			}
		}
		if len(creds) < 2 {
			out = append(out, proxyLine{Line: c.line, Err: fmt.Errorf("%w: no username and password after %s:%s", egress.ErrInvalidProxy, c.value, port)})
		} else {
			u, err := egress.ParseProxyLine(c.value + ":" + port + ":" + creds[0] + ":" + creds[1])
			out = append(out, proxyLine{Line: c.line, Proxy: u, Err: err})
		}
		i = j - 1
	}
	return out
}
