package execution

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSourceUploadResumesPartialChunk(t *testing.T) {
	for _, size := range []int{17, SourceChunkSize + 17} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			data := bytes.Repeat([]byte("synthetic bundle\n"), SourceChunkSize/8)
			p := f.s.sourcePath(f.r.Ref)
			if err := ownedDirectory(filepath.Dir(p)); err != nil {
				t.Fatal(err)
			}
			// Model the file left by an interrupted or short WriteAt: a
			// matching prefix ending partway through the current chunk.
			if err := os.WriteFile(p, data[:size], 0o600); err != nil {
				t.Fatal(err)
			}
			if err := f.transport(t, nil).UploadSource(ctx, f.r.Ref, data); err != nil {
				t.Fatalf("identical retry could not finish the partial chunk: %v", err)
			}
			if got, err := readRegular(p, MaxSourceBundle); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("resumed source differs from the original: %v", err)
			}
			if len(f.launcher.starts) != 0 {
				t.Fatal("source retry launched a harness")
			}
		})
	}
}

func TestSourcePartialChunkConflictPreservesBytes(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	first := SourceChunk{Total: 6, Data: []byte("ab")}
	if err := f.s.ReceiveSource(ctx, f.r.Ref, first); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReceiveSource(ctx, f.r.Ref, SourceChunk{Total: 6, Data: []byte("axcdef")}); err == nil {
		t.Fatal("conflicting partial chunk accepted")
	}
	if got, err := readRegular(f.s.sourcePath(f.r.Ref), MaxSourceBundle); err != nil || string(got) != "ab" {
		t.Fatalf("conflict modified the stored prefix: %q, %v", got, err)
	}
}
