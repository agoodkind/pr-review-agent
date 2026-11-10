package githubapp

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

const repositoryPackDrainBytes = 32 * 1024

func inspectRepositoryPack(ctx context.Context, source io.Reader) error {
	contextError := ctx.Err()
	if contextError != nil {
		return repositoryAccessError("cancel Git pack inspection", contextError)
	}

	scanner := packfile.NewScanner(source)

	_, count, err := scanner.Header()
	if err != nil {
		return repositoryAccessError("read Git pack header", err)
	}

	if count > maximumSnapshotEntries {
		return errRepositoryPackEntries
	}

	var representationBytes, resolvedBytes int64

	for range count {
		contextError = ctx.Err()
		if contextError != nil {
			return repositoryAccessError("cancel Git pack inspection", contextError)
		}

		header, headerError := scanner.NextObjectHeader()
		if headerError != nil {
			return repositoryAccessError("read Git object header", headerError)
		}

		if header.Length < 0 || header.Length > maximumSnapshotExtractedBytes-representationBytes {
			return errRepositoryPackBytes
		}

		representationBytes += header.Length

		sink := &repositoryPackSink{checkContext: ctx.Err, remaining: header.Length}
		if header.Type == plumbing.REFDeltaObject || header.Type == plumbing.OFSDeltaObject {
			resolvedBytes, err = inspectRepositoryDelta(scanner, sink, resolvedBytes)
		} else {
			if header.Length > maximumSnapshotExtractedBytes-resolvedBytes {
				return errRepositoryPackBytes
			}

			resolvedBytes += header.Length

			var written int64

			written, _, err = scanner.NextObject(sink)
			if err == nil && written != header.Length {
				return errRepositoryBlobSize
			}
		}

		if err != nil {
			return repositoryAccessError("inspect bounded Git object", err)
		}
	}

	_, err = scanner.Checksum()
	if err != nil {
		return repositoryAccessError("verify Git pack checksum", err)
	}

	return nil
}

func inspectRepositoryDelta(
	scanner *packfile.Scanner,
	sink *repositoryPackSink,
	resolvedBytes int64,
) (int64, error) {
	representation, err := scanner.ReadObject()
	if err != nil {
		return 0, repositoryAccessError("read bounded Git delta", err)
	}

	defer func() { _ = representation.Close() }()

	reader := bufio.NewReader(io.TeeReader(representation, sink))

	baseBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return 0, repositoryAccessError("read Git delta base size", err)
	}

	targetBytes, err := binary.ReadUvarint(reader)
	if err != nil {
		return 0, repositoryAccessError("read Git delta target size", err)
	}

	if baseBytes > maximumSnapshotExtractedBytes {
		return 0, errRepositoryPackBytes
	}

	if targetBytes > maximumSnapshotExtractedBytes {
		return 0, errRepositoryPackBytes
	}

	targetSize := int64(targetBytes)
	if targetSize > maximumSnapshotExtractedBytes-resolvedBytes {
		return 0, errRepositoryPackBytes
	}

	buffer := make([]byte, repositoryPackDrainBytes)

	for {
		contextError := sink.checkContext()
		if contextError != nil {
			return 0, repositoryAccessError("cancel Git delta inspection", contextError)
		}

		_, err = reader.Read(buffer)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return 0, repositoryAccessError("drain bounded Git delta", err)
		}
	}

	if sink.remaining != 0 {
		return 0, errRepositoryBlobSize
	}

	return resolvedBytes + targetSize, nil
}

type repositoryPackSink struct {
	checkContext func() error
	remaining    int64
}

func (sink *repositoryPackSink) Write(data []byte) (int, error) {
	contextError := sink.checkContext()
	if contextError != nil {
		return 0, repositoryAccessError("cancel Git object inspection", contextError)
	}

	if int64(len(data)) > sink.remaining {
		return 0, errRepositoryPackBytes
	}

	sink.remaining -= int64(len(data))

	return len(data), nil
}
