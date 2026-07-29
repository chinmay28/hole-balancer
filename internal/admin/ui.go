package admin

import (
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The management interface is embedded in the binary. Nothing is fetched from
// a CDN, and no assets sit on disk next to the executable: the page has to
// load on a network whose DNS is currently broken, which is precisely when
// somebody opens it.
var (
	//go:embed ui/index.html
	uiBody string
	//go:embed ui/app.css
	uiCSS string
	//go:embed ui/app.js
	uiJS string
)

// uiPage is assembled once at startup rather than per request.
var uiPage []byte

// uiCSP forbids this page from loading or contacting anything off-origin.
// Everything it needs is inline, so the policy can be this tight; if a future
// change needs an external asset, the policy failing is the correct outcome.
const uiCSP = "default-src 'none'; " +
	"style-src 'unsafe-inline'; " +
	"script-src 'unsafe-inline'; " +
	"connect-src 'self'; " +
	"img-src 'self' data:; " +
	"form-action 'none'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

func init() {
	var sb strings.Builder
	sb.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex, nofollow">
<title>hole-balancer</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Ccircle cx='16' cy='16' r='13' fill='none' stroke='%232a78d6' stroke-width='6'/%3E%3C/svg%3E">
<style>
`)
	sb.WriteString(uiCSS)
	sb.WriteString("\n</style>\n</head>\n<body>\n")
	sb.WriteString(uiBody)
	sb.WriteString("\n<script>\n")
	sb.WriteString(uiJS)
	sb.WriteString("\n</script>\n</body>\n</html>\n")
	uiPage = []byte(sb.String())
}

// handleUI serves the management dashboard.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	// Nothing here is a browsing history, but a management page has no business
	// being cached by an intermediary either.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", uiCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "index.html", startupTime, strings.NewReader(string(uiPage)))
}

// startupTime is the modification time reported for the embedded page, so a
// browser can revalidate it across a reload but a new build always wins.
var startupTime = time.Now()

// uiSize reports the assembled page size, for the startup log.
func uiSize() string { return fmt.Sprintf("%.0f KiB", float64(len(uiPage))/1024) }
