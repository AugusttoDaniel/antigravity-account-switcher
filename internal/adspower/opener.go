package adspower

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// devtoolsTarget is one entry of Chrome's /json target list.
type devtoolsTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Title                string `json:"title"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// ListPageTargets queries the browser's DevTools HTTP endpoint (exposed by ADS Power on the
// debug port returned by StartBrowser) and returns its open page targets. It is used to attach
// to the profile's existing tab instead of creating a new one.
func ListPageTargets(ctx context.Context, httpClient *http.Client, debugPort string) ([]devtoolsTarget, error) {
	debugPort = strings.TrimSpace(debugPort)
	if debugPort == "" {
		return nil, fmt.Errorf("adspower: empty debug port")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("adspower: build devtools request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("adspower: query devtools targets: %w", err)
	}
	defer resp.Body.Close()

	var all []devtoolsTarget
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, fmt.Errorf("adspower: decode devtools targets: %w", err)
	}

	pages := make([]devtoolsTarget, 0, len(all))
	for _, t := range all {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// pickPageTarget chooses the tab to drive: prefer a blank/new tab so we do not clobber a page
// the user is looking at; otherwise fall back to the first page.
func pickPageTarget(pages []devtoolsTarget) (devtoolsTarget, bool) {
	if len(pages) == 0 {
		return devtoolsTarget{}, false
	}
	for _, p := range pages {
		u := strings.TrimSpace(p.URL)
		if u == "" || u == "about:blank" || strings.HasPrefix(u, "chrome://newtab") || strings.HasPrefix(u, "edge://newtab") {
			return p, true
		}
	}
	return pages[0], true
}

// Navigate connects to the already-running profile browser and points an existing tab at
// targetURL, then disconnects. Because it attaches to a tab it did not create, disconnecting
// leaves the tab (and the browser window) open so the human can complete Google sign-in.
//
// browserWSURL is the browser-level CDP endpoint (BrowserStartData.WS.Puppeteer) and debugPort
// is BrowserStartData.DebugPort.
func Navigate(ctx context.Context, browserWSURL, debugPort, targetURL string) error {
	if strings.TrimSpace(browserWSURL) == "" {
		return fmt.Errorf("adspower: empty browser websocket url")
	}

	pages, err := ListPageTargets(ctx, nil, debugPort)
	if err != nil {
		return err
	}
	page, ok := pickPageTarget(pages)
	if !ok {
		return fmt.Errorf("adspower: profile browser has no page target to drive")
	}

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, browserWSURL)
	defer cancelAlloc()

	// Attaching to an existing target means chromedp will not close it when this context is
	// cancelled, so the consent tab survives our disconnect.
	attachCtx, cancelAttach := chromedp.NewContext(allocCtx, chromedp.WithTargetID(target.ID(page.ID)))
	defer cancelAttach()

	navCtx, cancelNav := context.WithTimeout(attachCtx, 45*time.Second)
	defer cancelNav()

	if err := chromedp.Run(navCtx, chromedp.Navigate(targetURL)); err != nil {
		return fmt.Errorf("adspower: navigate consent url in profile browser: %w", err)
	}
	return nil
}
