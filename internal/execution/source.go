package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
)

const MaxSourceBundle = 64 << 20
const SourceChunkSize = 256 << 10

// SourceUploader transfers coordinator-fetched objects before preparation.
// The digest and exact commit are bound into the durable start request.
type SourceUploader interface {
	UploadSource(context.Context, Ref, []byte) error
}

type SourceChunk struct {
	Offset int64  `json:"offset"`
	Total  int64  `json:"total"`
	Data   []byte `json:"data"`
}

func (s *Server) sourcePath(ref Ref) string {
	return filepath.Join(s.Root, "sources", string(ref.Execution), "source.bundle")
}

func (s *Server) ReceiveSource(ctx context.Context, ref Ref, chunk SourceChunk) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if chunk.Total <= 0 || chunk.Total > MaxSourceBundle || chunk.Offset < 0 || chunk.Offset >= chunk.Total || len(chunk.Data) == 0 || len(chunk.Data) > SourceChunkSize || int64(len(chunk.Data)) > chunk.Total-chunk.Offset {
		return errors.New("invalid source chunk")
	}
	lock, _, err := s.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	// Once preparation exists, its source is immutable, including retries.
	prepared := false
	if _, err := s.Read(ref); err == nil {
		prepared = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p := s.sourcePath(ref)
	if err := ownedDirectory(filepath.Dir(p)); err != nil {
		return err
	}
	if info, err := os.Lstat(p); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("source transfer is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	flags := os.O_CREATE | os.O_RDWR
	if prepared {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(filepath.Clean(p), flags, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if prepared && info.Size() != chunk.Total {
		return errors.New("execution source is already prepared")
	}
	if info.Size() < chunk.Offset || info.Size() > chunk.Total {
		return errors.New("out-of-order source chunk")
	}
	if info.Size() > chunk.Offset {
		old := make([]byte, len(chunk.Data))
		if _, err := f.ReadAt(old, chunk.Offset); err != nil || !bytes.Equal(old, chunk.Data) {
			return errors.New("conflicting source chunk retry")
		}
		return nil
	}
	if _, err := f.WriteAt(chunk.Data, chunk.Offset); err != nil {
		return err
	}
	return f.Sync()
}

func (t SSHTransport) UploadSource(ctx context.Context, ref Ref, data []byte) error {
	if len(data) == 0 || len(data) > MaxSourceBundle {
		return errors.New("invalid source bundle size")
	}
	for offset := 0; offset < len(data); offset += SourceChunkSize {
		var result struct{}
		chunk := SourceChunk{Offset: int64(offset), Total: int64(len(data)), Data: data[offset:min(offset+SourceChunkSize, len(data))]}
		if err := t.Call(ctx, "source-chunk", ref, chunk, &result); err != nil {
			return err
		}
	}
	return nil
}
