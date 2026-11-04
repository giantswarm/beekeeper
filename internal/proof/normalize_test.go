package proof

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"
)

// A compressed file's byte count does not bound the decoded pixel buffer.
// Keep this fixture modest even if the pre-decode check regresses.
func TestNormalizePixelBudget(t *testing.T) {
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewGray(image.Rect(0, 0, 8192, 4097))); err != nil {
		t.Fatal(err)
	}
	if raw.Len() > MaxBytes {
		t.Fatalf("fixture is %d bytes, outside the encoded limit", raw.Len())
	}
	out, err := Normalize(raw.Bytes())
	if err == nil || !strings.Contains(err.Error(), "pixels") || len(out) != 0 {
		t.Fatalf("Normalize(%d compressed bytes, 8192x4097) returned %d bytes, %v; want pixel-limit refusal", raw.Len(), len(out), err)
	}
}

// These inputs end after their size headers. A pixel-limit error, rather
// than a missing-image-data error, proves refusal happens before decoding.
func TestNormalizeOversizedHeaders(t *testing.T) {
	p := testPNG(t)[:33] // signature and IHDR only
	binary.BigEndian.PutUint32(p[16:20], 16384)
	binary.BigEndian.PutUint32(p[20:24], 8192)
	binary.BigEndian.PutUint32(p[29:33], crc32.ChecksumIEEE(p[12:29]))
	fixtures := map[string][]byte{
		"png": p,
		"jpeg": {
			0xff, 0xd8, // SOI
			0xff, 0xe0, 0, 7, 'J', 'F', 'I', 'F', 0, // JFIF
			0xff, 0xc0, 0, 11, 8, 0xff, 0xff, 0xff, 0xff, 1, 1, 0x11, 0, // 65535x65535, grayscale
		},
		"gif": append([]byte("GIF89a"), 0xff, 0xff, 0xff, 0xff, 0, 0, 0),
	}
	for name, raw := range fixtures {
		t.Run(name, func(t *testing.T) {
			if _, _, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil {
				t.Fatalf("fixture has no valid dimensions: %v", err)
			}
			out, err := Normalize(raw)
			if err == nil || !strings.Contains(err.Error(), "pixels") || len(out) != 0 {
				t.Fatalf("Normalize returned %d bytes, %v; want pixel-limit refusal", len(out), err)
			}
		})
	}
}

func TestNormalizeGIF(t *testing.T) {
	src := image.NewPaletted(image.Rect(0, 0, 4, 3), color.Palette{color.Black, color.White})
	src.SetColorIndex(1, 1, 1)
	var raw bytes.Buffer
	if err := gif.Encode(&raw, src, nil); err != nil {
		t.Fatal(err)
	}
	out, err := Normalize(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != src.Bounds() || color.GrayModel.Convert(img.At(1, 1)) != (color.Gray{Y: 255}) {
		t.Fatalf("GIF dimensions or pixels changed: %v, %v", img.Bounds(), img.At(1, 1))
	}
}
