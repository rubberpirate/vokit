package vivo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// IncomingShare follows app-connection.js SHARE_TRANS_FILE:/FILE_TRANS_TAG: and
// renderer.js startOriginIslandDownload; remote paths never select local paths.
type IncomingShare struct {
	Files          []FileEntry
	FromSharePanel bool
}

func ParseIncomingShare(data []byte) (IncomingShare, bool, error) {
	var result IncomingShare
	prefix := "SHARE_TRANS_FILE:"
	if !strings.HasPrefix(string(data), prefix) {
		prefix = "FILE_TRANS_TAG:"
		if !strings.HasPrefix(string(data), prefix) {
			return result, false, nil
		}
	} else {
		result.FromSharePanel = true
	}
	if len(data) > 256*1024 || !utf8.Valid(data) {
		return result, true, errors.New("incoming share exceeds metadata limit")
	}
	var entries []struct {
		Path        string `json:"path"`
		FileName    string `json:"fileName"`
		FileSize    int64  `json:"fileSize"`
		IsDirectory bool   `json:"isDirectory"`
	}
	if json.Unmarshal(data[len(prefix):], &entries) != nil || len(entries) == 0 || len(entries) > 200 {
		return result, true, errors.New("invalid incoming share metadata")
	}
	var total int64
	for _, e := range entries {
		entry := FileEntry{SavePath: e.Path, FileName: e.FileName, FileSize: e.FileSize, IsDirectory: e.IsDirectory}
		if !validFileEntry(entry) || entry.IsDirectory || entry.FileSize > MaxDownloadBytes || len(entry.FileName) > 200 || strings.ContainsAny(entry.FileName, "\r\n") {
			return IncomingShare{}, true, errors.New("unsupported incoming share selection")
		}
		total += entry.FileSize
		if total > 4*MaxDownloadBytes {
			return IncomingShare{}, true, errors.New("incoming share exceeds batch limit")
		}
		result.Files = append(result.Files, entry)
	}
	return result, true, nil
}

// PrepareReceiveDirectory creates only user-selected directory components and
// refuses symbolic links, including intermediate components.
func PrepareReceiveDirectory(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" || strings.ContainsAny(directory, "\x00\\") {
		return errors.New("receive directory must be a normalized absolute directory")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot open receive directory")
	}
	defer func() { syscall.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(directory, "/"), "/") {
		if err = syscall.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, syscall.EEXIST) {
			return errors.New("cannot create receive directory")
		}
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if e != nil {
			return errors.New("receive directory traverses a symlink or is unavailable")
		}
		syscall.Close(fd)
		fd = next
	}
	return nil
}

func (p *PhoneClient) ReceiveSharedFile(ctx context.Context, s *Session, entry FileEntry, directory string, fromSharePanel bool) (DownloadResult, error) {
	if err := PrepareReceiveDirectory(directory); err != nil {
		return DownloadResult{}, err
	}
	kind := "FROM_PC_FILE_MANAGER"
	if fromSharePanel {
		kind = "FROM_VIVO_SHARE_FILE"
	}
	result, err := p.downloadFile(ctx, s, entry, filepath.Join(directory, entry.FileName), kind)
	if !errors.Is(err, ErrDownloadExists) {
		return result, err
	}
	for range 4 {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return DownloadResult{}, errors.New("cannot choose receive filename")
		}
		extension := filepath.Ext(entry.FileName)
		name := strings.TrimSuffix(entry.FileName, extension) + " (" + hex.EncodeToString(random) + ")" + extension
		result, err = p.downloadFile(ctx, s, entry, filepath.Join(directory, name), kind)
		if !errors.Is(err, ErrDownloadExists) {
			return result, err
		}
	}
	return DownloadResult{}, ErrDownloadExists
}
