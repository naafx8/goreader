package main

import (
	"bufio"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"io"
	"strings"
	"unicode/utf8"

	_ "image/jpeg"
	_ "image/png"

	"github.com/nfnt/resize"
	termbox "github.com/nsf/termbox-go"
	"github.com/olekukonko/tablewriter"
	"github.com/taylorskalyo/goreader/epub"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type parser struct {
	tagStack   []atom.Atom
	tableStack [][][]string
	tokenizer  *html.Tokenizer
	doc        cellbuf
	items      []epub.Item
}

type cellbuf struct {
	cells   []termbox.Cell
	width   int
	lmargin int
	col     int
	row     int
	fg, bg  termbox.Attribute
}

// setCell changes a cell's attributes in the cell buffer document at the given
// position.
func (c *cellbuf) setCell(x, y int, ch rune, fg, bg termbox.Attribute) {
	// Grow in steps of 1024 when out of space.
	for y*c.width+x >= len(c.cells) {
		c.cells = append(c.cells, make([]termbox.Cell, 1024)...)
	}
	c.cells[y*c.width+x] = termbox.Cell{Ch: ch, Fg: fg, Bg: bg}
}

// scanWords is a split function for a Scanner that returns space-separated
// words. Unlike bufio.ScanWords(), scanWords only splits on spaces (i.e. not
// newlines, tabs, or other whitespace).
func scanWords(data []byte, atEOF bool) (advance int, token []byte, err error) {
	// Skip leading spaces.
	start := 0
	for width := 0; start < len(data); start += width {
		var r rune
		r, width = utf8.DecodeRune(data[start:])
		if r != ' ' {
			break
		}
	}

	// Scan until space, marking end of word.
	for width, i := 0, start; i < len(data); i += width {
		var r rune
		r, width = utf8.DecodeRune(data[i:])
		if r == ' ' {
			return i + width, data[start:i], nil
		}
	}

	// If we're at EOF, we have a final, non-empty, non-terminated word. Return
	// it.
	if atEOF && len(data) > start {
		return len(data), data[start:], nil
	}

	// Request more data.
	return start, nil, nil
}

// style sets the foreground/background attributes for future cells in the cell
// buffer document based on HTML tags in the tag stack.
func (c *cellbuf) style(tags []atom.Atom) {
	fg := termbox.ColorDefault
	for _, tag := range tags {
		switch tag {
		case atom.B, atom.Strong, atom.Em:
			fg |= termbox.AttrBold
		case atom.I:
			fg |= termbox.ColorYellow
		case atom.Title:
			fg |= termbox.ColorRed
		case atom.H1:
			fg |= termbox.ColorMagenta
		case atom.H2:
			fg |= termbox.ColorBlue
		case atom.H3, atom.H4, atom.H5, atom.H6:
			fg |= termbox.ColorCyan
		}
	}
	c.fg = fg
}

// appendText appends text to the cell buffer document. Long lines are
// word-wrapped.
func (c *cellbuf) appendText(str string) {
	if c.col < c.lmargin {
		c.col = c.lmargin
	}
	scanner := bufio.NewScanner(strings.NewReader(str))
	scanner.Split(scanWords)
	for scanner.Scan() {
		word := []rune(scanner.Text())
		if len(word) > c.width-c.col {
			c.row++
			c.col = c.lmargin
		}
		for _, r := range word {
			if r == '\n' {
				c.row++
				c.col = c.lmargin
				continue
			}
			c.setCell(c.col, c.row, r, c.fg, c.bg)
			c.col++
		}
		if c.col != c.lmargin {
			c.col++
		}
	}
}

// appendText appends text to the curently focussed text element.
func (p *parser) appendText(str string, raw bool) {
	if cell := p.getTableCell(); cell != nil {
		*cell += strings.TrimSpace(str)
		return
	}
	if raw {
		p.doc.appendRaw(str)
		return
	}
	p.doc.appendText(str)
}

// getTableCell returns a pointer to the currently focussed table cell, if one
// exists in the table stack.
func (p parser) getTableCell() *string {
	if len(p.tableStack) > 0 {
		tableI := len(p.tableStack) - 1
		if len(p.tableStack[tableI]) > 0 {
			rowI := len(p.tableStack[tableI]) - 1
			if len(p.tableStack[tableI][rowI]) > 0 {
				colI := len(p.tableStack[tableI][rowI]) - 1
				return &p.tableStack[tableI][rowI][colI]
			}
		}
	}
	return nil
}

// appendRaw appends a raw string to the cell buffer document.
func (c *cellbuf) appendRaw(str string) {
	if c.col < c.lmargin {
		c.col = c.lmargin
	}
	for _, r := range str {
		if r == '\n' {
			c.row++
			c.col = c.lmargin
			continue
		}
		c.setCell(c.col, c.row, r, c.fg, c.bg)
		c.col++
	}
}

// parseText takes in html content via an io.Reader and returns a buffer
// containing only plain text.
func parseText(r io.Reader, items []epub.Item) (cellbuf, error) {
	tokenizer := html.NewTokenizer(r)
	doc := cellbuf{width: 80}
	p := parser{tokenizer: tokenizer, doc: doc, items: items}
	err := p.parse(r)
	if err != nil {
		return p.doc, err
	}
	return p.doc, nil
}

// parse walks an html document and renders elements to a cell buffer document.
func (p *parser) parse(io.Reader) (err error) {
	for {
		tokenType := p.tokenizer.Next()
		token := p.tokenizer.Token()
		switch tokenType {
		case html.ErrorToken:
			err = p.tokenizer.Err()
		case html.StartTagToken:
			p.tagStack = append(p.tagStack, token.DataAtom) // push element
			fallthrough
		case html.SelfClosingTagToken:
			p.handleStartTag(token)
		case html.TextToken:
			p.handleText(token)
		case html.EndTagToken:
			p.handleEndTag(token)
			p.tagStack = p.tagStack[:len(p.tagStack)-1] // pop element
		}
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// handleText appends text elements to the parser buffer. It filters elements
// that should not be displayed as text (e.g. style blocks).
func (p *parser) handleText(token html.Token) {
	// Skip style tags
	if len(p.tagStack) > 0 && p.tagStack[len(p.tagStack)-1] == atom.Style {
		return
	}
	p.doc.style(p.tagStack)
	p.appendText(string(token.Data), false)
}

// handleStartTag appends text representations of non-text elements (e.g. ASCII
// art and alt text for images). It also builds complex structures, like
// tables, by placing the fragmentary pieces on a stack.
func (p *parser) handleStartTag(token html.Token) {
	switch token.DataAtom {
	case atom.Img:
		// Display alt text in place of images.
		for _, a := range token.Attr {
			switch atom.Lookup([]byte(a.Key)) {
			case atom.Alt:
				text := fmt.Sprintf("Alt text: %s\n", a.Val)
				p.appendText(text, false)
			case atom.Src:
				// In order to display images inside tables, we need to know the column
				// width, which isn't known (if it's available through the tablewriter
				// API at all) until the whole table has been traversed. For now, just
				// skip rendering images inside tables.
				if cell := p.getTableCell(); cell != nil {
					break
				}
				for _, item := range p.items {
					if item.HREF == a.Val {
						p.appendText(imageToText(item), true)
						break
					}
				}
			}
		}
	case atom.Br:
		p.appendText("\n", true)
	case atom.P:
		p.doc.col += 2
	case atom.Hr:
		p.doc.col = 0
		p.appendText(strings.Repeat("-", p.doc.width), true)
	case atom.Table:
		p.tableStack = append(p.tableStack, [][]string{})
	case atom.Tr:
		tableI := len(p.tableStack) - 1
		p.tableStack[tableI] = append(p.tableStack[tableI], []string{})
	case atom.Td:
		tableI := len(p.tableStack) - 1
		rowI := len(p.tableStack[tableI]) - 1
		p.tableStack[tableI][rowI] = append(p.tableStack[tableI][rowI], "")
	}
}

// handleEndTag appends text representations of complex structures, like
// tables, by piecing together fragments from a stack.
func (p *parser) handleEndTag(token html.Token) {
	switch token.DataAtom {
	case atom.Table:
		tableI := len(p.tableStack) - 1
		buf := bytes.NewBufferString("")
		table := tablewriter.NewWriter(buf)
		table.AppendBulk(p.tableStack[tableI])
		table.SetBorder(true)
		table.SetRowLine(true)
		table.Render()
		p.tableStack = p.tableStack[:tableI]
		p.appendText("\n", true)
		p.appendText(buf.String(), true)
	}
}

// imageToText opens an image reference and converts it to ASCII art.
func imageToText(item epub.Item) string {
	r, err := item.Open()
	if err != nil {
		return ""
	}

	img, _, err := image.Decode(r)
	if err != nil {
		return ""
	}
	bounds := img.Bounds()

	// Assume a character height to width ratio of 2:1.
	w := 80
	h := (bounds.Max.Y * w) / (bounds.Max.X * 2)
	img = resize.Resize(uint(w), uint(h), img, resize.Lanczos3)

	charGradient := []rune("MND8OZ$7I?+=~:,..")
	buf := new(bytes.Buffer)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.GrayModel.Convert(img.At(x, y))
			y := c.(color.Gray).Y
			pos := (len(charGradient) - 1) * int(y) / 255
			buf.WriteRune(charGradient[pos])
		}
		buf.WriteRune('\n')
	}

	return buf.String()
}
