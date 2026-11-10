package feedwatch

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
)

const (
	nsAtom    = "http://www.w3.org/2005/Atom"
	nsRSS1    = "http://purl.org/rss/1.0/"
	nsContent = "http://purl.org/rss/1.0/modules/content/"
	nsDC      = "http://purl.org/dc/elements/1.1/"
)

// item is one feed entry, whatever format it came in.
type item struct {
	ID         string // guid / atom id, may be empty
	Title      string
	Link       string
	HTML       string // content as published (usually HTML)
	Author     string
	Categories []string
	Enclosures []string
	Published  time.Time // zero when missing or unparseable
	RawDate    string
}

type feed struct {
	Title string
	Link  string
	Items []item // in document order (normally newest first)
}

// el captures an element whose meaning depends on its namespace or
// attributes: <link>text</link> vs <atom:link href>, <title> vs <media:title>.
type el struct {
	XMLName xml.Name
	Href    string `xml:"href,attr"`
	Rel     string `xml:"rel,attr"`
	Type    string `xml:"type,attr"`
	Term    string `xml:"term,attr"`
	URL     string `xml:"url,attr"`
	Text    string `xml:",chardata"`
	Inner   string `xml:",innerxml"`
}

type rssItem struct {
	Titles       []el   `xml:"title"`
	Links        []el   `xml:"link"`
	Descriptions []el   `xml:"description"`
	Encoded      string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	GUID         string `xml:"guid"`
	PubDate      string `xml:"pubDate"`
	DCDate       string `xml:"http://purl.org/dc/elements/1.1/ date"`
	Authors      []el   `xml:"author"`
	Creator      string `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Categories   []el   `xml:"category"`
	Enclosures   []el   `xml:"enclosure"`
}

type atomEntry struct {
	ID        string `xml:"http://www.w3.org/2005/Atom id"`
	Titles    []el   `xml:"http://www.w3.org/2005/Atom title"`
	Links     []el   `xml:"http://www.w3.org/2005/Atom link"`
	Summary   *el    `xml:"http://www.w3.org/2005/Atom summary"`
	Content   *el    `xml:"http://www.w3.org/2005/Atom content"`
	Published string `xml:"http://www.w3.org/2005/Atom published"`
	Updated   string `xml:"http://www.w3.org/2005/Atom updated"`
	Authors   []struct {
		Name string `xml:"http://www.w3.org/2005/Atom name"`
	} `xml:"http://www.w3.org/2005/Atom author"`
	Categories []el `xml:"http://www.w3.org/2005/Atom category"`
}

// document covers RSS 2.0 / 0.9x (<rss><channel><item>), RSS 1.0
// (<rdf:RDF><channel/><item>) and Atom (<feed><entry>) in one shape.
type document struct {
	XMLName xml.Name
	Channel struct {
		Titles []el      `xml:"title"`
		Links  []el      `xml:"link"`
		Items  []rssItem `xml:"item"`
	} `xml:"channel"`
	Items   []rssItem   `xml:"item"`
	Titles  []el        `xml:"title"`
	Links   []el        `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

// parseFeed reads an RSS or Atom document. base is the feed's own URL, used
// to resolve relative links.
func parseFeed(data []byte, base string) (*feed, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(label)
		if err != nil {
			return nil, fmt.Errorf("不支持的编码 %s", label)
		}
		return enc.NewDecoder().Reader(r), nil
	}
	var doc document
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.New("不是有效的 RSS / Atom 文档：" + err.Error())
	}
	baseURL, _ := url.Parse(base)
	abs := func(ref string) string {
		ref = strings.TrimSpace(ref)
		if ref == "" || baseURL == nil {
			return ref
		}
		if u, err := url.Parse(ref); err == nil {
			return baseURL.ResolveReference(u).String()
		}
		return ref
	}

	out := &feed{}
	switch strings.ToLower(doc.XMLName.Local) {
	case "feed":
		out.Title, out.Link = plain(doc.Titles), abs(atomLink(doc.Links))
		for _, e := range doc.Entries {
			it := item{ID: strings.TrimSpace(e.ID), Title: plain(e.Titles), Link: abs(atomLink(e.Links))}
			switch {
			case e.Content != nil && strings.TrimSpace(atomText(e.Content)) != "":
				it.HTML = atomText(e.Content)
			case e.Summary != nil:
				it.HTML = atomText(e.Summary)
			}
			if len(e.Authors) > 0 {
				it.Author = strings.TrimSpace(e.Authors[0].Name)
			}
			for _, c := range e.Categories {
				if t := strings.TrimSpace(c.Term); t != "" {
					it.Categories = append(it.Categories, t)
				}
			}
			for _, l := range e.Links {
				if l.Rel == "enclosure" && l.Href != "" {
					it.Enclosures = append(it.Enclosures, abs(l.Href))
				}
			}
			it.RawDate = strings.TrimSpace(e.Published)
			if it.RawDate == "" {
				it.RawDate = strings.TrimSpace(e.Updated)
			}
			it.Published = parseDate(it.RawDate)
			out.Items = append(out.Items, it)
		}
	case "rss", "rdf":
		out.Title, out.Link = plain(doc.Channel.Titles), abs(rssLink(doc.Channel.Links))
		for _, r := range append(doc.Channel.Items, doc.Items...) {
			it := item{ID: strings.TrimSpace(r.GUID), Title: plain(r.Titles), Link: abs(rssLink(r.Links))}
			it.HTML = r.Encoded
			if strings.TrimSpace(it.HTML) == "" {
				if d := native(r.Descriptions); d != nil {
					it.HTML = d.Text
				}
			}
			if a := native(r.Authors); a != nil {
				it.Author = strings.TrimSpace(a.Text)
			}
			if it.Author == "" {
				it.Author = strings.TrimSpace(r.Creator)
			}
			for _, c := range r.Categories {
				if t := strings.TrimSpace(c.Text); t != "" && c.XMLName.Space == "" {
					it.Categories = append(it.Categories, t)
				}
			}
			for _, e := range r.Enclosures {
				if e.URL != "" {
					it.Enclosures = append(it.Enclosures, abs(e.URL))
				}
			}
			it.RawDate = strings.TrimSpace(r.PubDate)
			if it.RawDate == "" {
				it.RawDate = strings.TrimSpace(r.DCDate)
			}
			it.Published = parseDate(it.RawDate)
			out.Items = append(out.Items, it)
		}
	default:
		return nil, fmt.Errorf("不是 RSS / Atom 文档（根元素是 <%s>）", doc.XMLName.Local)
	}
	return out, nil
}

// native picks the element that belongs to the feed format itself rather than
// to an extension such as media: or itunes:.
func native(els []el) *el {
	for i := range els {
		switch els[i].XMLName.Space {
		case "", nsRSS1, nsAtom:
			return &els[i]
		}
	}
	return nil
}

func plain(els []el) string {
	e := native(els)
	if e == nil {
		return ""
	}
	if e.Type == "html" || e.Type == "xhtml" || strings.ContainsAny(e.Text, "<&") {
		return oneLine(htmlToText(atomText(e)))
	}
	return oneLine(e.Text)
}

func atomText(e *el) string {
	if e.Type == "xhtml" {
		return e.Inner
	}
	return e.Text
}

func rssLink(els []el) string {
	for _, e := range els {
		if e.XMLName.Space != nsAtom && strings.TrimSpace(e.Text) != "" {
			return e.Text
		}
	}
	return atomLink(els)
}

func atomLink(els []el) string {
	fallback := ""
	for _, e := range els {
		if e.Href == "" {
			continue
		}
		if e.Rel == "alternate" || e.Rel == "" {
			return e.Href
		}
		if fallback == "" && e.Rel != "self" && e.Rel != "enclosure" {
			fallback = e.Href
		}
	}
	return fallback
}

var dateLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano, time.RFC822Z, time.RFC822,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
}

func parseDate(s string) time.Time {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

var (
	reDrop  = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)>|<!--.*?-->`)
	reBreak = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6]|blockquote|pre)>`)
	reTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reImg   = regexp.MustCompile(`(?is)<img\b[^>]*?\bsrc\s*=\s*["']([^"']+)["']`)
	reBlank = regexp.MustCompile(`\n{3,}`)
	reSpace = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
)

// htmlToText reduces HTML to readable plain text: enough for a chat message
// or a prompt, not a faithful rendering.
func htmlToText(s string) string {
	s = reDrop.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func images(s string) []string {
	var out []string
	for _, m := range reImg.FindAllStringSubmatch(s, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
