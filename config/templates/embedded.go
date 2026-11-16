// Package templates renders review output from Markdown templates.
package templates

import (
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"text/template"
)

// Files contains the review output templates.
//
//go:embed *.md.tmpl
var Files embed.FS

var (
	errOutputRender = errors.New("render output template")
	loadTemplates   = sync.OnceValues(func() (*template.Template, error) {
		return template.New("output").Option("missingkey=error").ParseFS(Files, "output.*.md.tmpl")
	})
)

// Render formats output with the selected Markdown template.
//
// Missing templates and invalid data stop rendering with an error.
//
// For example:
//
//	data := reflect.ValueOf(result)
//	text, err := templates.Render("output.multiline_result", data)
func Render(name string, data reflect.Value) (string, error) {
	compiled, err := loadTemplates()
	if err != nil {
		slog.Warn("parse output templates", slog.String("err", err.Error()))

		return "", fmt.Errorf("%w %q: %w", errOutputRender, name, err)
	}

	var rendered strings.Builder
	if err := compiled.ExecuteTemplate(&rendered, name, data); err != nil {
		slog.Warn(
			"execute output template",
			slog.String("template", name),
			slog.String("err", err.Error()),
		)

		return "", fmt.Errorf("%w %q: %w", errOutputRender, name, err)
	}

	return strings.TrimSuffix(rendered.String(), "\n"), nil
}
