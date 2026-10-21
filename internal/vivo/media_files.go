package vivo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const MaxThumbnailBytes int64 = maxResponseBytes
const MaxImagePreviewBytes int64 = 32 << 20
const MaxVideoPreviewBytes int64 = 256 << 20

type Thumbnail struct {
	Data   []byte `json:"data"`
	MIME   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type MediaPreview struct {
	Path   string `json:"path"`
	MIME   string `json:"mime"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// FetchThumbnail uses the near-phone GET contract at renderer.js 369707.
// Caller must obtain file consent and select this entry from phone metadata.
func (p *PhoneClient) FetchThumbnail(ctx context.Context, s *Session, entry FileEntry, width, height int) (Thumbnail, error) {
	if s == nil || s.Token == "" || !validFileEntry(entry) || entry.IsDirectory || entry.IsGroup || width < 1 || height < 1 || width > 1024 || height > 1024 {
		return Thumbnail{}, errors.New("invalid thumbnail selection or dimensions")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	query := url.Values{"fileUri": {entry.SavePath}, "width": {strconv.Itoa(width)}, "height": {strconv.Itoa(height)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/pc_file_manager/thumb?"+query.Encode(), nil)
	if err != nil {
		return Thumbnail{}, errors.New("cannot build thumbnail request")
	}
	req.Header.Set("newToken", s.Token)
	req.Header.Set("X-ES-HTTP-VERSION", "1")
	req.Header.Set("Connection", "keep-alive")
	response, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Thumbnail{}, ctx.Err()
		}
		return Thumbnail{}, errors.New("phone thumbnail request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == 445 || response.StatusCode == http.StatusForbidden {
			return Thumbnail{}, ErrFilePermission
		}
		return Thumbnail{}, errors.New("phone rejected thumbnail")
	}
	if response.ContentLength > MaxThumbnailBytes {
		return Thumbnail{}, errors.New("thumbnail exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxThumbnailBytes+1))
	if ctx.Err() != nil {
		return Thumbnail{}, ctx.Err()
	}
	if err != nil || int64(len(data)) > MaxThumbnailBytes {
		return Thumbnail{}, errors.New("invalid or oversized thumbnail response")
	}
	contentType, dimensions, err := imageMediaType(bytes.NewReader(data), 4096, 16<<20)
	if err != nil {
		return Thumbnail{}, errors.New("thumbnail is not a supported bounded image")
	}
	declared := response.Header.Get("Content-Type")
	if declared != "" {
		parsed, _, err := mime.ParseMediaType(declared)
		if err != nil || parsed != contentType && parsed != "application/octet-stream" {
			return Thumbnail{}, errors.New("thumbnail content type does not match its bytes")
		}
	}
	return Thumbnail{Data: data, MIME: contentType, Width: dimensions.Width, Height: dimensions.Height}, nil
}

// PrepareMediaPreview downloads the whole selected file to a new private cache
// path, then validates media type. Failed type checks can leave the downloaded
// cache file for caller cleanup; it must never be opened as a successful preview.
func (p *PhoneClient) PrepareMediaPreview(ctx context.Context, s *Session, entry FileEntry, outputPath string) (MediaPreview, error) {
	limit := mediaPreviewLimit(entry.FileName)
	if s == nil || s.Token == "" || !validFileEntry(entry) || entry.IsDirectory || entry.IsGroup || limit == 0 || entry.FileSize <= 0 || entry.FileSize > limit {
		return MediaPreview{}, errors.New("invalid media selection or preview size")
	}
	result, err := p.DownloadFile(ctx, s, entry, outputPath)
	if err != nil {
		return MediaPreview{}, err
	}
	file, info, err := openUploadFile(outputPath)
	if err != nil || info.Size() != result.Bytes {
		if file != nil {
			file.Close()
		}
		return MediaPreview{}, errors.New("cannot validate downloaded preview")
	}
	defer file.Close()
	header := make([]byte, 512)
	n, readErr := io.ReadFull(file, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return MediaPreview{}, errors.New("cannot read downloaded preview")
	}
	header = header[:n]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return MediaPreview{}, errors.New("cannot inspect downloaded preview")
	}
	var contentType string
	if limit == MaxImagePreviewBytes {
		contentType, _, err = imageMediaType(file, 32768, 100<<20)
	} else {
		contentType = videoMediaType(header)
		if contentType == "" {
			err = errors.New("unsupported video container")
		}
	}
	if err != nil {
		return MediaPreview{}, errors.New("downloaded bytes are not supported preview media")
	}
	// Recheck the stable descriptor before exposing its path to a local viewer.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return MediaPreview{}, errors.New("cannot verify downloaded preview")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, &fileContextReader{ctx: ctx, reader: io.LimitReader(file, limit+1)})
	if ctx.Err() != nil {
		return MediaPreview{}, ctx.Err()
	}
	if err != nil || written != result.Bytes || hex.EncodeToString(hash.Sum(nil)) != result.SHA256 {
		return MediaPreview{}, errors.New("downloaded preview changed during validation")
	}
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return MediaPreview{}, errors.New("invalid preview cache path")
	}
	return MediaPreview{Path: absolute, MIME: contentType, Bytes: result.Bytes, SHA256: result.SHA256}, nil
}

func mediaPreviewLimit(name string) int64 {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif":
		return MaxImagePreviewBytes
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".3gp":
		return MaxVideoPreviewBytes
	}
	return 0
}

func imageMediaType(reader io.Reader, maxDimension, maxPixels int) (string, image.Config, error) {
	config, format, err := image.DecodeConfig(reader)
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxDimension || config.Height > maxDimension || int64(config.Width)*int64(config.Height) > int64(maxPixels) {
		return "", image.Config{}, errors.New("invalid image dimensions or format")
	}
	if format != "png" && format != "jpeg" && format != "gif" {
		return "", image.Config{}, errors.New("unsupported image format")
	}
	return "image/" + format, config, nil
}

func videoMediaType(header []byte) string {
	if len(header) >= 12 && string(header[4:8]) == "ftyp" {
		switch string(header[8:12]) {
		case "qt  ":
			return "video/quicktime"
		case "3gp4", "3gp5", "3gp6", "3ge6", "3gg6":
			return "video/3gpp"
		case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "M4V ", "MSNV", "dash":
			return "video/mp4"
		}
	}
	if len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "AVI " {
		return "video/avi"
	}
	if bytes.HasPrefix(header, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		if bytes.Contains(header, []byte("webm")) {
			return "video/webm"
		}
		if bytes.Contains(header, []byte("matroska")) {
			return "video/x-matroska"
		}
	}
	return ""
}
