package review

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type inspectionHTMLWriter struct {
	root         atom.Atom
	active       bool
	finished     bool
	rootDepth    int
	omitted      atom.Atom
	omittedDepth int
	preDepth     int
	output       strings.Builder
}

func inspectionHTMLText(source string) (string, error) {
	root, err := inspectionHTMLRoot(source)
	if err != nil {
		return "", err
	}

	writer := inspectionHTMLWriter{
		root:         root,
		active:       root == 0,
		finished:     false,
		rootDepth:    0,
		omitted:      0,
		omittedDepth: 0,
		preDepth:     0,
		output:       strings.Builder{},
	}
	tokenizer := html.NewTokenizer(strings.NewReader(source))

	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if err := tokenizer.Err(); !errors.Is(err, io.EOF) {
				slog.Warn("Read public inspection HTML text failed", "err", err)
				return "", fmt.Errorf("read public inspection HTML text: %w", err)
			}

			break
		}

		token := tokenizer.Token()
		if writer.selectRoot(kind, token.DataAtom) {
			if writer.finished {
				break
			}

			continue
		}

		if !writer.skipToken(kind, token.DataAtom) {
			writer.writeToken(kind, token)
		}
	}

	text := strings.TrimSpace(writer.output.String())
	if text == "" {
		return "", errors.New("the public source HTML did not contain readable text")
	}

	return text, nil
}

func inspectionHTMLRoot(source string) (atom.Atom, error) {
	root := atom.Atom(0)
	tokenizer := html.NewTokenizer(strings.NewReader(source))

	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if err := tokenizer.Err(); !errors.Is(err, io.EOF) {
				slog.Warn("Read public inspection HTML structure failed", "err", err)
				return 0, fmt.Errorf("read public inspection HTML structure: %w", err)
			}

			return root, nil
		}

		if kind != html.StartTagToken {
			continue
		}

		tag := tokenizer.Token().DataAtom
		if tag == atom.Main {
			return atom.Main, nil
		}

		if tag == atom.Body {
			root = atom.Body
		}
	}
}

func (writer *inspectionHTMLWriter) selectRoot(kind html.TokenType, tag atom.Atom) bool {
	if !writer.active {
		if kind == html.StartTagToken && tag == writer.root {
			writer.active = true
			writer.rootDepth = 1
		}

		return true
	}

	if writer.root == 0 || tag != writer.root {
		return false
	}

	if kind == html.StartTagToken {
		writer.rootDepth++
	}

	if kind == html.EndTagToken {
		writer.rootDepth--
		writer.finished = writer.rootDepth == 0
	}

	return writer.finished
}

func (writer *inspectionHTMLWriter) skipToken(kind html.TokenType, tag atom.Atom) bool {
	if writer.omitted != 0 {
		if tag == writer.omitted && kind == html.StartTagToken {
			writer.omittedDepth++
		}

		if tag == writer.omitted && kind == html.EndTagToken {
			writer.omittedDepth--
			if writer.omittedDepth == 0 {
				writer.omitted = 0
			}
		}

		return true
	}

	if kind == html.StartTagToken && inspectionHTMLOmitted(tag) {
		writer.omitted = tag
		writer.omittedDepth = 1

		return true
	}

	return false
}

func inspectionHTMLOmitted(tag atom.Atom) bool {
	return slices.Contains(
		[]atom.Atom{
			atom.Head,
			atom.Script,
			atom.Style,
			atom.Nav,
			atom.Header,
			atom.Footer,
			atom.Noscript,
		},
		tag,
	)
}

func (writer *inspectionHTMLWriter) writeToken(kind html.TokenType, token html.Token) {
	switch kind {
	case html.TextToken:
		writer.writeText(token.Data)
	case html.StartTagToken, html.SelfClosingTagToken:
		if token.DataAtom == atom.Br || inspectionHTMLBlock(token.DataAtom) {
			writer.output.WriteByte('\n')
		}

		if kind == html.StartTagToken && token.DataAtom == atom.Pre {
			writer.preDepth++
		}
	case html.EndTagToken:
		if token.DataAtom == atom.Pre && writer.preDepth > 0 {
			writer.preDepth--
		}

		if inspectionHTMLBlock(token.DataAtom) {
			writer.output.WriteByte('\n')
		}
	case html.ErrorToken, html.CommentToken, html.DoctypeToken:
		return
	}
}

func (writer *inspectionHTMLWriter) writeText(text string) {
	if writer.preDepth > 0 {
		writer.output.WriteString(text)
		return
	}

	first, _ := utf8.DecodeRuneInString(text)
	last, _ := utf8.DecodeLastRuneInString(text)

	if unicode.IsSpace(first) {
		writer.output.WriteByte(' ')
	}

	writer.output.WriteString(strings.Join(strings.Fields(text), " "))

	if unicode.IsSpace(last) {
		writer.output.WriteByte(' ')
	}
}

func inspectionHTMLBlock(tag atom.Atom) bool {
	return slices.Contains(
		[]atom.Atom{
			atom.P,
			atom.Div,
			atom.Section,
			atom.Article,
			atom.H1,
			atom.H2,
			atom.H3,
			atom.H4,
			atom.H5,
			atom.H6,
			atom.Li,
			atom.Ul,
			atom.Ol,
			atom.Table,
			atom.Tr,
			atom.Pre,
			atom.Blockquote,
			atom.Dl,
			atom.Dt,
			atom.Dd,
		},
		tag,
	)
}
