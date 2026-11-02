package text

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// HTMLToText converts Slack canvas HTML into readable plain text while keeping
// headings, paragraphs and list items on separate lines.
func HTMLToText(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return raw
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}

		switch n.Data {
		case "script", "style":
			return
		case "h1", "h2", "h3", "h4", "h5", "h6":
			b.WriteString("\n\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			b.WriteString("\n\n")
		case "p", "div", "section", "article", "blockquote":
			b.WriteString("\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			b.WriteString("\n")
		case "li":
			b.WriteString("\n- ")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			b.WriteString("\n")
		case "br":
			b.WriteString("\n")
		case "img":
			if alt := attr(n, "alt"); alt != "" {
				b.WriteString(alt)
			}
		case "control":
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(doc)

	lines := strings.Split(b.String(), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	return regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
