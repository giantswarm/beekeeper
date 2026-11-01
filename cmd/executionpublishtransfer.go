package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const publicationTransfer = "publish-transfer"
const publicationChunkSize = 256 << 10

type publicationChunk struct {
	Digest   string `json:"digest"`
	Offset   int64  `json:"offset"`
	Total    int64  `json:"total"`
	Data     []byte `json:"data,omitempty"`
	Download bool   `json:"download,omitempty"`
}

func (a *app) publicationTransferPath(ref execution.Ref, digest string) (string, error) {
	decoded, decodeErr := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if ref.Validate() != nil || !strings.HasPrefix(digest, "sha256:") || decodeErr != nil || len(decoded) != 32 {
		return "", refused("invalid publication transfer identity")
	}
	return filepath.Join(a.cfg.StateDir, "execution-publications", "transfers", string(ref.Execution), fmt.Sprint(ref.Generation), digest[7:]), nil
}

func (a *app) transferPublication(st *state.State, ref execution.Ref, p publicationChunk) (publicationChunk, error) {
	result := publicationChunk{Digest: p.Digest, Offset: p.Offset, Total: p.Total}
	if _, err := a.executionPublicationLive(st, ref); err != nil {
		return result, err
	}
	if p.Total <= 0 || p.Total > publishBundleLimit || p.Offset < 0 || p.Offset >= p.Total || len(p.Data) > publicationChunkSize {
		return result, refused("invalid publication chunk bounds")
	}
	path, err := a.publicationTransferPath(ref, p.Digest)
	if err != nil {
		return result, err
	}
	if p.Download {
		if len(p.Data) != 0 {
			return result, refused("download cannot upload data")
		}
		f, openErr := os.Open(filepath.Clean(path))
		if openErr != nil {
			return result, openErr
		}
		defer func() { _ = f.Close() }()
		info, statErr := f.Stat()
		if statErr != nil || info.Size() != p.Total {
			return result, refused("publication transfer size changed")
		}
		result.Data = make([]byte, min(int64(publicationChunkSize), p.Total-p.Offset))
		_, err = f.ReadAt(result.Data, p.Offset)
		return result, err
	}
	if len(p.Data) == 0 || p.Offset+int64(len(p.Data)) > p.Total {
		return result, refused("invalid upload length")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return result, err
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return result, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return result, err
	}
	if info.Size() < p.Offset || info.Size() > p.Total {
		return result, refused("out-of-order publication transfer")
	}
	if info.Size() > p.Offset {
		old := make([]byte, len(p.Data))
		if _, err := f.ReadAt(old, p.Offset); err != nil || !bytes.Equal(old, p.Data) {
			return result, refused("conflicting publication chunk retry")
		}
	} else {
		if _, err := f.WriteAt(p.Data, p.Offset); err != nil {
			return result, err
		}
		if err := f.Sync(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (a *app) loadPublicationTransfer(ref execution.Ref, digest string) ([]byte, error) {
	path, err := a.publicationTransferPath(ref, digest)
	if err != nil {
		return nil, err
	}
	data, err := readPublicationBundle(path)
	if err != nil {
		return nil, err
	}
	if execution.Digest(data) != digest {
		return nil, refused("publication transfer is incomplete or corrupt")
	}
	return data, nil
}

func (a *app) storePublicationTransfer(ref execution.Ref, data []byte) (string, error) {
	digest := execution.Digest(data)
	path, err := a.publicationTransferPath(ref, digest)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	err = os.WriteFile(path, data, 0o600)
	return digest, err
}

func publicationTransferCall(ctx context.Context, s *execution.Server, j execution.Journal, p publicationChunk) (publicationChunk, error) {
	var result publicationChunk
	co := s.Config.Coordinators[j.Request.Coordinator]
	err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), publicationTransfer, j.Handle.Ref, p, j.Request.ControlKey, &result, s.Callback)
	return result, err
}

func uploadPublication(ctx context.Context, s *execution.Server, j execution.Journal, data []byte) (string, error) {
	digest := execution.Digest(data)
	for offset := 0; offset < len(data); offset += publicationChunkSize {
		p := publicationChunk{Digest: digest, Offset: int64(offset), Total: int64(len(data)), Data: data[offset:min(offset+publicationChunkSize, len(data))]}
		if _, err := publicationTransferCall(ctx, s, j, p); err != nil {
			return "", err
		}
	}
	return digest, nil
}

func downloadPublication(ctx context.Context, s *execution.Server, j execution.Journal, digest string, total int64) ([]byte, error) {
	if total <= 0 || total > publishBundleLimit {
		return nil, errors.New("invalid publication download size")
	}
	var data bytes.Buffer
	for offset := int64(0); offset < total; {
		result, err := publicationTransferCall(ctx, s, j, publicationChunk{Digest: digest, Offset: offset, Total: total, Download: true})
		if err != nil {
			return nil, err
		}
		if result.Digest != digest || result.Offset != offset || result.Total != total || len(result.Data) == 0 || int64(len(result.Data)) > total-offset {
			return nil, io.ErrUnexpectedEOF
		}
		_, _ = data.Write(result.Data)
		offset += int64(len(result.Data))
	}
	if execution.Digest(data.Bytes()) != digest {
		return nil, errors.New("publication download digest mismatch")
	}
	return data.Bytes(), nil
}
