package vivo

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const MaxDownloadBytes int64 = 1 << 30

var ErrDownloadExists = errors.New("download destination already exists")

type FileEntry struct {
	FileName    string `json:"fileName"`
	SavePath    string `json:"savePath"`
	FileSize    int64  `json:"fileSize"`
	Date        int64  `json:"date"`
	IsDirectory bool   `json:"isDirectory"`
	IsGroup     bool   `json:"isGroup,omitempty"`
	GroupName   string `json:"groupName,omitempty"`
	GroupTime   string `json:"groupTime,omitempty"`
}

type FilePage struct {
	DataList   []FileEntry `json:"dataList"`
	TotalCount int64       `json:"totalCount"`
}

type DownloadResult struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func validPhonePath(value string) bool {
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

func validFileEntry(entry FileEntry) bool {
	return validPhonePath(entry.SavePath) && entry.FileName != "" && entry.FileName != "." && entry.FileName != ".." &&
		!strings.ContainsAny(entry.FileName, "/\\\x00") && (entry.IsDirectory || path.Base(entry.SavePath) == entry.FileName) && entry.FileSize >= 0
}

// Contract: shipped app-modules.js character offsets 5008, 66800–71180, 305209–310900.
func (p *PhoneClient) ListFiles(ctx context.Context, s *Session, directory string, page int) (FilePage, error) {
	var result FilePage
	if s == nil || s.Token == "" || !validPhonePath(directory) || page < 0 {
		return result, errors.New("invalid file listing request")
	}
	// Vendor breadcrumbs use '/' for innerRoot, physical paths for children.
	if directory == s.StorageRoot {
		directory = "/"
	}
	body := map[string]any{"category": "", "data": directory, "fileCount": 0, "sortCondition": 0, "groupBy": 0, "type": "REQUEST_POSTS_FILELIST", "pageIndex": page, "pageNumber": 200, "versionTag": 1}
	if err := p.post(ctx, "/pc_file_manager/channel", s, body, 30*time.Second, &result); err != nil {
		return FilePage{}, err
	}
	if result.DataList == nil || result.TotalCount < 0 {
		return FilePage{}, errors.New("invalid file listing response")
	}
	entries := result.DataList[:0]
	for _, entry := range result.DataList {
		// app-modules.js 1267281 separates group headings from actual entries.
		if entry.IsGroup {
			continue
		}
		if !validFileEntry(entry) {
			return FilePage{}, errors.New("invalid file metadata")
		}
		entries = append(entries, entry)
	}
	result.DataList = entries
	return result, nil
}

// Contract: shipped main.js character offsets 4474800–4481300; the body is TAR.
// DownloadFile writes one selected regular file and never replaces a destination.
func (p *PhoneClient) DownloadFile(ctx context.Context, s *Session, entry FileEntry, outputPath string) (DownloadResult, error) {
	return p.downloadFile(ctx, s, entry, outputPath, "FROM_PC_FILE_MANAGER")
}

func (p *PhoneClient) downloadFile(ctx context.Context, s *Session, entry FileEntry, outputPath, transferType string) (DownloadResult, error) {
	var result DownloadResult
	if s == nil || s.Token == "" || !validFileEntry(entry) || entry.IsDirectory || entry.FileSize > MaxDownloadBytes {
		return result, errors.New("invalid download selection")
	}
	parent, basename, err := openDownloadDirectory(outputPath)
	if err != nil {
		return result, err
	}
	defer parent.Close()
	var destinationStat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), basename, &destinationStat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return result, ErrDownloadExists
	} else if !errors.Is(err, unix.ENOENT) {
		return result, errors.New("download destination is unavailable")
	}
	// Anchor all destination operations to the opened directory, even if renamed.
	stage, stageName, err := createDownloadStage(parent)
	if err != nil {
		return result, errors.New("cannot create download staging file")
	}
	defer unix.Unlinkat(int(parent.Fd()), stageName, 0)
	defer stage.Close()
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return result, errors.New("cannot create download identifier")
	}
	idBytes[6] = idBytes[6]&15 | 64
	idBytes[8] = idBytes[8]&63 | 128
	id := fmt.Sprintf("%x-%x-%x-%x-%x", idBytes[:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:])
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	body := map[string]any{"downloadList": []string{entry.SavePath}, "type": transferType, "total": entry.FileSize, "isDir": false, "isAlbum": false, "albumList": []string{}, "dirList": []string{}}
	// main.js 4476500 accepts HTTP 200; counts are optional progress metadata.
	// Exact entry name/type/size are enforced on the received TAR instead.
	if err := p.post(ctx, "/pc_file_manager/download_info?id="+id, s, body, 30*time.Second, nil); err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/pc_file_manager/download?id="+id, nil)
	if err != nil {
		return result, errors.New("cannot build download request")
	}
	req.Header.Set("newToken", s.Token)
	req.Header.Set("X-ES-HTTP-VERSION", "1")
	req.Header.Set("Connection", "keep-alive")
	response, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.New("phone download request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == 445 || response.StatusCode == http.StatusForbidden {
			return result, ErrFilePermission
		}
		return result, errors.New("phone rejected download")
	}
	result, err = readSingleFileTar(ctx, response.Body, entry, stage)
	if err != nil {
		return DownloadResult{}, err
	}
	if err := stage.Sync(); err != nil {
		return DownloadResult{}, errors.New("cannot persist download")
	}
	if err := ctx.Err(); err != nil {
		return DownloadResult{}, err
	}
	if err := unix.Linkat(int(parent.Fd()), stageName, int(parent.Fd()), basename, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return DownloadResult{}, ErrDownloadExists
		}
		return DownloadResult{}, errors.New("cannot commit download without replacing destination")
	}
	return result, nil
}

func readSingleFileTar(ctx context.Context, body io.Reader, entry FileEntry, output io.Writer) (DownloadResult, error) {
	// Allow bounded TAR headers and padding in addition to the selected file.
	limited := &io.LimitedReader{R: body, N: entry.FileSize + (1 << 20) + 1}
	reader := tar.NewReader(limited)
	header, err := reader.Next()
	if err != nil || header.Typeflag != tar.TypeReg || header.Name != entry.FileName || strings.ContainsAny(header.Name, "/\\\x00") || header.Size != entry.FileSize {
		return DownloadResult{}, errors.New("invalid download archive or file size")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), &fileContextReader{ctx: ctx, reader: reader})
	if err != nil || written != entry.FileSize {
		if ctx.Err() != nil {
			return DownloadResult{}, ctx.Err()
		}
		return DownloadResult{}, errors.New("incomplete download")
	}
	if _, err := reader.Next(); err != io.EOF {
		return DownloadResult{}, errors.New("download archive contains unexpected entries or is invalid")
	}
	// TAR readers stop at the zero terminator; reject hidden archives/data after it.
	buffer := make([]byte, 4096)
	for {
		n, err := (&fileContextReader{ctx: ctx, reader: limited}).Read(buffer)
		for _, b := range buffer[:n] {
			if b != 0 {
				return DownloadResult{}, errors.New("download archive has unexpected trailing data")
			}
		}
		if limited.N == 0 {
			return DownloadResult{}, errors.New("download archive exceeds size limit")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return DownloadResult{}, ctx.Err()
			}
			return DownloadResult{}, errors.New("cannot finish download archive")
		}
	}
	return DownloadResult{Bytes: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

type fileContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *fileContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}

func openDownloadDirectory(output string) (*os.File, string, error) {
	if output == "" || strings.ContainsAny(output, "\x00") || strings.HasSuffix(output, "/") {
		return nil, "", errors.New("invalid download destination")
	}
	for _, part := range strings.Split(output, string(filepath.Separator)) {
		if part == ".." {
			return nil, "", errors.New("invalid download destination")
		}
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return nil, "", errors.New("invalid download destination")
	}
	absolute = macOSPhysicalPath(absolute)
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", errors.New("cannot open download directory")
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(absolute), "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := openDirectoryAt(fd, part)
		unix.Close(fd)
		if openErr != nil {
			return nil, "", errors.New("download directory is unavailable or traverses a symlink")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), "download-directory"), filepath.Base(absolute), nil
}

// macOS exposes /tmp and /var as system symlink aliases. Map only those fixed
// OS aliases to their physical paths; all user-selected path components are
// still checked with O_NOFOLLOW during the descriptor walk.
func macOSPhysicalPath(path string) string {
	if runtime.GOOS != "darwin" {
		return path
	}
	if strings.HasPrefix(path, "/tmp/") || path == "/tmp" {
		return "/private" + path
	}
	if strings.HasPrefix(path, "/var/") || path == "/var" {
		return "/private" + path
	}
	return path
}

func openDirectoryAt(parentFD int, name string) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return -1, unix.ENOTDIR
	}
	return fd, nil
}

func createDownloadStage(parent *os.File) (*os.File, string, error) {
	for range 10 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", err
		}
		name := ".vivo-download-" + hex.EncodeToString(random)
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return os.NewFile(uintptr(fd), name), name, nil
	}
	return nil, "", errors.New("cannot allocate staging filename")
}
