package vivo

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// StreamMediaType accepts only regular media selected from a phone listing.
func StreamMediaType(entry FileEntry) (string, error) {
	if !validFileEntry(entry) || entry.IsDirectory || entry.IsGroup || entry.FileSize <= 0 || entry.FileSize > MaxDownloadBytes {
		return "", errors.New("invalid streaming media selection")
	}
	ext := strings.ToLower(filepath.Ext(entry.FileName))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".heif", ".avif", ".bmp":
		if entry.FileSize > MaxImagePreviewBytes {
			return "", errors.New("image exceeds preview limit")
		}
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".3gp":
	default:
		return "", errors.New("unsupported streaming media")
	}
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		return "", errors.New("unknown media type")
	}
	return contentType, nil
}

// OpenMediaStream uses the direct raw-media route recovered from the vendor's
// image viewer (originDownloadPath). It does not prepare a TAR or save a file.
// The caller owns the response body and must close it before closing this client.
func (p *PhoneClient) OpenMediaStream(ctx context.Context, s *Session, entry FileEntry, byteRange string) (*http.Response, error) {
	if _, err := StreamMediaType(entry); err != nil {
		return nil, err
	}
	if s == nil || s.Token == "" {
		return nil, errors.New("media session required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/download/down_files?"+url.Values{"path": {entry.SavePath}}.Encode(), nil)
	if err != nil {
		return nil, errors.New("invalid media request")
	}
	req.Header.Set("newToken", s.Token)
	req.Header.Set("X-ES-HTTP-VERSION", "1")
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	response, err := p.http.Do(req)
	if err != nil {
		return nil, errors.New("phone media stream unavailable")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		response.Body.Close()
		if response.StatusCode == 445 || response.StatusCode == http.StatusForbidden {
			return nil, ErrFilePermission
		}
		return nil, errors.New("phone rejected media stream")
	}
	return response, nil
}
