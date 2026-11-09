// Package testarchive writes repository archives for integration fixtures.
package testarchive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
)

// Write creates a compressed tar archive of fixture entries.
//
// The function sorts entry names and returns archive or compression errors.
//
// For example:
//
//	err := Write(writer, "repository", contents)
func Write(writer io.Writer, root string, contents map[string]string) (writeErr error) {
	defer func() {
		if writeErr != nil {
			slog.Warn("Write repository fixture archive failed", "err", writeErr)
			writeErr = fmt.Errorf("write repository fixture archive: %w", writeErr)
		}
	}()
	compressed := gzip.NewWriter(writer)
	defer func() {
		if err := compressed.Close(); err != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("close fixture compression: %w", err))
		}
	}()
	archive := tar.NewWriter(compressed)
	defer func() {
		if err := archive.Close(); err != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("close fixture tar archive: %w", err))
		}
	}()
	var rootHeader tar.Header
	rootHeader.Name = root
	rootHeader.Mode = 0o755
	rootHeader.Typeflag = tar.TypeDir
	if err := archive.WriteHeader(&rootHeader); err != nil {
		return fmt.Errorf("write fixture archive root: %w", err)
	}
	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		content := contents[name]
		var header tar.Header
		header.Name = root + name
		header.Mode = 0o644
		header.Size = int64(len(content))
		header.Typeflag = tar.TypeReg
		if strings.HasSuffix(name, "/") {
			header.Mode = 0o755
			header.Size = 0
			header.Typeflag = tar.TypeDir
		}
		if err := archive.WriteHeader(&header); err != nil {
			return fmt.Errorf("write fixture archive entry %q: %w", name, err)
		}
		if _, err := io.WriteString(archive, content); err != nil {
			return fmt.Errorf("write fixture archive entry data %q: %w", name, err)
		}
	}
	return nil
}
