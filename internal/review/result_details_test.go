package review_test

import (
	"bytes"
	"errors"
	stdhtml "html"
	"io"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"

	"goodkind.io/pr-review-agent/internal/review"
)

func TestReviewDetailsRenderReturnedResult(t *testing.T) {
	const failure = "file assessment validation failed: code=assessment_persistence_limit field= observed=83647 limit=48000"

	for _, result := range []string{"", failure} {
		var summary review.Summary
		summary.Result = result
		body := review.RenderDetails(summary)
		expected := result
		if expected == "" {
			expected = "null"
		}

		if !strings.Contains(body, "| `result` | `"+expected+"` |") {
			t.Fatalf("Review details omitted the returned result: %s", body)
		}
	}
}

func TestReviewDetailsPreserveMultilineErrorText(t *testing.T) {
	const failure = "first\terror: `value` | <input>\r\nsecond error: &details *stars* **bold** _under_ [label](https://example.invalid) \\*escaped* ~~deleted~~ https://example.invalid test@example.invalid www.example.invalid"
	var summary review.Summary
	summary.Result = failure
	body := review.RenderDetails(summary)
	_, encoded, found := strings.Cut(body, "| `result` | <pre>")
	if !found {
		t.Fatalf("Review details omitted the multiline error: %s", body)
	}

	encoded, _, found = strings.Cut(encoded, "</pre> |")
	if !found || stdhtml.UnescapeString(encoded) != failure ||
		strings.ContainsAny(encoded, "\r\n") {
		t.Fatalf("Review details changed the error text: %q", encoded)
	}

	markdown := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
	var rendered bytes.Buffer
	if err := markdown.Convert([]byte(body), &rendered); err != nil {
		t.Fatal(err)
	}

	tokenizer := html.NewTokenizer(&rendered)
	insideResult := false
	resultCount := 0
	var literal strings.Builder

	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if !errors.Is(tokenizer.Err(), io.EOF) {
				t.Fatal(tokenizer.Err())
			}

			break
		}

		token := tokenizer.Token()
		if kind == html.EndTagToken && token.Data == "pre" {
			insideResult = false

			continue
		}

		if insideResult {
			if kind != html.TextToken {
				t.Fatalf("The result contains interpreted markup: %s", token.String())
			}

			literal.WriteString(token.Data)
		}

		if kind == html.StartTagToken && token.Data == "pre" {
			insideResult = true
			resultCount++
		}
	}

	expected := strings.ReplaceAll(failure, "\r\n", "\n")
	if resultCount != 1 || literal.String() != expected {
		t.Fatalf("The rendered result changed the error text: %q", literal.String())
	}
}
