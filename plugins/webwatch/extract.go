package main

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// Elements whose text is never shown to a reader.
var hidden = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Head: true, atom.Svg: true, atom.Iframe: true,
}

// Elements that start a new line.
var block = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Li: true, atom.Tr: true, atom.Br: true, atom.Hr: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Blockquote: true, atom.Pre: true, atom.Section: true, atom.Article: true,
	atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Aside: true, atom.Main: true,
	atom.Ul: true, atom.Ol: true, atom.Table: true, atom.Dt: true, atom.Dd: true,
	atom.Form: true, atom.Option: true, atom.Figure: true, atom.Figcaption: true,
}

// extract reduces a response body to the lines of text that are compared
// between checks. For HTML that is the visible text of the elements matching
// sel (the whole body when sel is nil); anything else is compared as text.
func extract(body []byte, contentType string, sel cascadia.Matcher, ignore []*regexp.Regexp) (title string, lines []string, err error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return "", nil, err
	}
	decoded, err := io.ReadAll(r)
	if err != nil {
		return "", nil, err
	}
	if !isHTML(decoded, contentType) {
		if sel != nil {
			return "", nil, errors.New("不是 HTML 页面，不能使用选择器")
		}
		return "", clean(string(decoded), ignore), nil
	}
	doc, err := html.Parse(bytes.NewReader(decoded))
	if err != nil {
		return "", nil, err
	}
	if t := cascadia.Query(doc, titleSel); t != nil {
		var b strings.Builder
		render(&b, t)
		title = strings.Join(strings.Fields(b.String()), " ")
	}
	roots := []*html.Node{doc}
	if sel != nil {
		matched := cascadia.QueryAll(doc, sel)
		if len(matched) == 0 {
			// Not a change: the site may be serving an error or a login page.
			return title, nil, errors.New("选择器没有匹配到任何内容")
		}
		in := make(map[*html.Node]bool, len(matched))
		for _, n := range matched {
			in[n] = true
		}
		roots = roots[:0]
		for _, n := range matched {
			if !hasAncestor(n, in) { // nested matches would repeat the text
				roots = append(roots, n)
			}
		}
	}
	var b strings.Builder
	for _, n := range roots {
		if sel == nil {
			walk(&b, n)
		} else {
			render(&b, n)
		}
		b.WriteByte('\n')
	}
	return title, clean(b.String(), ignore), nil
}

var titleSel = cascadia.MustCompile("head > title")

func isHTML(body []byte, contentType string) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "html") {
		return true
	}
	if ct != "" && !strings.HasPrefix(ct, "text/plain") && !strings.HasPrefix(ct, "application/octet-stream") {
		return false // JSON, XML, CSV…
	}
	head := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

func hasAncestor(n *html.Node, in map[*html.Node]bool) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if in[p] {
			return true
		}
	}
	return false
}

// walk writes the visible text below n, n included.
func walk(b *strings.Builder, n *html.Node) {
	if n.Type == html.ElementNode && hidden[n.DataAtom] {
		return
	}
	render(b, n)
}

// render writes the text of n even when n itself is a hidden element, so a
// selector can point at <title> or a <script type="application/json">.
func render(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.CommentNode, html.DoctypeNode:
		return
	}
	brk := n.Type == html.ElementNode && block[n.DataAtom]
	if brk {
		b.WriteByte('\n')
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(b, c)
	}
	switch {
	case brk:
		b.WriteByte('\n')
	case n.DataAtom == atom.Td || n.DataAtom == atom.Th:
		b.WriteByte(' ')
	}
}

// clean splits text into lines, drops what the user asked to ignore, collapses
// whitespace and removes blank lines, so cosmetic changes don't count.
func clean(text string, ignore []*regexp.Regexp) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, re := range ignore {
			line = re.ReplaceAllString(line, "")
		}
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// diffLines reports the lines only in cur and the lines only in old, each in
// its original order. A line that merely moved is in neither.
func diffLines(old, cur []string) (added, removed []string) {
	only := func(a, b []string) []string {
		left := make(map[string]int, len(b))
		for _, l := range b {
			left[l]++
		}
		var out []string
		for _, l := range a {
			if left[l] > 0 {
				left[l]--
			} else {
				out = append(out, l)
			}
		}
		return out
	}
	return only(cur, old), only(old, cur)
}
