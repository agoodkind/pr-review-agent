package githubapp_test

import (
	"bytes"
	"compress/zlib"
	"crypto"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	githash "github.com/go-git/go-git/v5/plumbing/hash"
)

type gitPackFixtureObject struct {
	header  []byte
	payload []byte
}

func gitPackFixture(t *testing.T, objects []gitPackFixtureObject) []byte {
	t.Helper()

	var pack bytes.Buffer
	pack.WriteString("PACK")

	writeError := binary.Write(&pack, binary.BigEndian, uint32(2))
	if writeError != nil {
		t.Fatal(writeError)
	}

	writeError = binary.Write(&pack, binary.BigEndian, uint32(len(objects)))
	if writeError != nil {
		t.Fatal(writeError)
	}

	for _, object := range objects {
		pack.Write(object.header)
		compressed := zlib.NewWriter(&pack)

		_, writeError = compressed.Write(object.payload)
		if writeError != nil {
			t.Fatal(writeError)
		}

		closeError := compressed.Close()
		if closeError != nil {
			t.Fatal(closeError)
		}
	}

	checksum := githash.New(crypto.SHA1)

	_, writeError = checksum.Write(pack.Bytes())
	if writeError != nil {
		t.Fatal(writeError)
	}

	return append(pack.Bytes(), checksum.Sum(nil)...)
}

// TestGitRejectsDeltaTargetBeforeApplication verifies that a tiny encoded delta
// cannot declare an excessive resolved object before the SDK applies it.
func TestGitRejectsDeltaTargetBeforeApplication(t *testing.T) {
	fixture := newGitFixture(t)
	base := []byte("x")
	hasher := plumbing.NewHasher(plumbing.BlobObject, int64(len(base)))

	_, err := hasher.Write(base)
	if err != nil {
		t.Fatal(err)
	}

	baseHash := hasher.Sum()
	deltaHeader := append([]byte{0x76}, baseHash[:]...)
	fixture.responsePack = gitPackFixture(
		t,
		[]gitPackFixtureObject{
			{header: []byte{0x31}, payload: base},
			{header: deltaHeader, payload: []byte{1, 0x81, 0x80, 0x80, 0x80, 0x08}},
		},
	)

	_, err = fixture.client.GetFile(
		t.Context(),
		77,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if err == nil || !strings.Contains(err.Error(), "extracted byte limit") {
		t.Fatalf("oversized delta target did not stop before application: %v", err)
	}
}

// TestGitBoundsActualInflation verifies that a valid compressed stream cannot
// inflate beyond its smaller declared representation size.
func TestGitBoundsActualInflation(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.responsePack = gitPackFixture(
		t,
		[]gitPackFixtureObject{
			{header: []byte{0x31}, payload: []byte("xx")},
		},
	)

	_, err := fixture.client.GetFile(
		t.Context(),
		77,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if err == nil || !strings.Contains(err.Error(), "declared size") {
		t.Fatalf("actual inflation did not respect the declared byte bound: %v", err)
	}
}
