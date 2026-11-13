// Package orgrender turns org text into HTML using go-org.
package orgrender

import (
	"fmt"
	"html/template"
	"io"
	"log"
	"strings"

	"github.com/niklasfasching/go-org/org"
)

// Resolver maps an org link to a URL inside the app. protocol is "file", "id"
// or "" (a bare path); it returns "" to leave the link as written.
type Resolver func(protocol, target string) string

// HTML renders org source. toc controls the table of contents go-org adds for
// documents with headings; resolve may be nil. Rendering never reads other files (#+INCLUDE is
// refused). Raw HTML export blocks pass through; the files are the owner's
// own, and the site's Content-Security-Policy blocks inline scripts anyway.
func HTML(src string, toc bool, resolve Resolver) template.HTML {
	conf := org.New()
	conf.Log = log.New(io.Discard, "", 0)
	conf.ReadFile = func(string) ([]byte, error) { return nil, fmt.Errorf("includes are disabled") }
	conf.ResolveLink = func(protocol string, description []org.Node, link string) org.Node {
		if resolve != nil {
			if u := resolve(protocol, strings.TrimPrefix(link, protocol+":")); u != "" {
				// A protocol go-org does not know, so it leaves the URL alone.
				return org.RegularLink{Protocol: "app", Description: description, URL: u}
			}
		}
		return org.RegularLink{Protocol: protocol, Description: description, URL: link}
	}
	settings := map[string]string{}
	for k, v := range conf.DefaultSettings {
		settings[k] = v
	}
	if !toc {
		settings["OPTIONS"] = strings.Replace(settings["OPTIONS"], "toc:t", "toc:nil", 1)
	}
	conf.DefaultSettings = settings
	w := org.NewHTMLWriter()
	// go-org's default highlighter emits inline styles, which the CSP blocks.
	w.HighlightCodeBlock = func(source, lang string, inline bool, params map[string]string) string {
		esc := template.HTMLEscapeString(source)
		if inline {
			return "<code>" + esc + "</code>"
		}
		return `<div class="highlight"><pre>` + esc + `</pre></div>`
	}
	out, err := conf.Parse(strings.NewReader(src), "").Write(w)
	if err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(out)
}
