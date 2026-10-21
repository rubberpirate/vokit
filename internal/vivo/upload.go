package vivo

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const MaxUploadBytes int64 = 1 << 30

type UploadResult struct {
	FileName    string `json:"fileName"`
	SavePath    string `json:"savePath,omitempty"`
	Bytes       int64  `json:"bytes"`
	LocalSHA256 string `json:"local_sha256"`
}

// UploadFile creates a new randomly named file, requesting rename on collision.
// LocalSHA256 describes bytes read locally; the protocol supplies no peer hash.
func (p *PhoneClient) UploadFile(ctx context.Context, s *Session, localPath, remoteDirectory string) (result UploadResult, err error) {
	phase := "source"
	defer func() {
		if err != nil {
			err = &FileOperationError{Phase: phase, Err: err}
		}
	}()
	if s == nil || s.Token == "" || !validPhonePath(remoteDirectory) {
		return UploadResult{}, errors.New("invalid upload request")
	}
	file, info, err := openUploadFile(localPath)
	if err != nil {
		return UploadResult{}, err
	}
	defer file.Close()
	phase = "destination"
	remoteDirectory, err = p.ResolveUploadDirectory(ctx, s, remoteDirectory)
	if err != nil {
		return UploadResult{}, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return UploadResult{}, errors.New("cannot create upload identifier")
	}
	name := "vivo-transfer-" + hex.EncodeToString(random) + uploadExtension(localPath)
	random[6] = random[6]&15 | 64
	random[8] = random[8]&63 | 128
	id := fmt.Sprintf("%x-%x-%x-%x-%x", random[:4], random[4:6], random[6:8], random[8:10], random[10:])
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// Shipped main.js 4481900–4483500 builds metadata; 4528620+ posts it.
	body := map[string]any{
		"id": id, "dropFileItems": []map[string]any{{
			"index": -1, "isDirectory": false, "fileName": name, "fileSize": info.Size(), "savePath": "",
			"date": info.ModTime().UnixMilli(), "fileType": "UNKNOWN", "mimeType": "application/octet-stream", "ifDuplicated": "rename", "fileReturn": false,
		}},
		"totalSize": info.Size(), "totalCount": 1, "screen_w": 0, "screen_h": 0, "x": 0, "y": 0,
		"type": "TO_PC_FILE_MANAGER", "savePath": remoteDirectory,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return UploadResult{}, errors.New("cannot encode upload metadata")
	}
	prepCtx, prepCancel := context.WithTimeout(ctx, 30*time.Second)
	req, err := p.uploadRequest(prepCtx, "/pc_file_manager/drop_files_info?id="+id, s, bytes.NewReader(encoded), "application/json")
	if err != nil {
		prepCancel()
		return UploadResult{}, err
	}
	phase = "preparation"
	response, err := p.http.Do(req)
	if err != nil {
		prepCancel()
		if ctx.Err() != nil {
			return UploadResult{}, ctx.Err()
		}
		return UploadResult{}, errors.New("upload preparation failed")
	}
	_, err = readFileAcknowledgment(response)
	prepCancel()
	if err != nil {
		return UploadResult{}, err
	}
	reader, writer := io.Pipe()
	req, err = p.uploadRequest(ctx, "/upload/drop_file_to_phone?id="+id+"&type=tar", s, reader, "application/x-tar")
	if err != nil {
		reader.Close()
		writer.Close()
		return UploadResult{}, err
	}
	type streamedResult struct {
		bytes int64
		hash  string
		err   error
	}
	streamed := make(chan streamedResult, 1)
	go func() {
		hash := sha256.New()
		archive := tar.NewWriter(writer)
		err := archive.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: info.Size(), ModTime: info.ModTime()})
		var written int64
		if err == nil {
			written, err = io.CopyN(io.MultiWriter(archive, hash), &fileContextReader{ctx: ctx, reader: file}, info.Size())
		}
		if err == nil {
			var extra [1]byte
			n, readErr := file.Read(extra[:])
			after, statErr := file.Stat()
			if n != 0 || readErr != io.EOF || statErr != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				err = errors.New("upload source changed")
			}
		}
		if err == nil {
			err = archive.Close()
		}
		writer.CloseWithError(err)
		streamed <- streamedResult{bytes: written, hash: hex.EncodeToString(hash.Sum(nil)), err: err}
	}()
	stop := context.AfterFunc(ctx, func() {
		reader.CloseWithError(ctx.Err())
		writer.CloseWithError(ctx.Err())
	})
	defer stop()
	// An early peer reply must also stop and join the TAR producer.
	phase = "stream"
	response, requestErr := p.http.Do(req)
	reader.CloseWithError(io.ErrClosedPipe)
	finished := <-streamed
	if ctx.Err() != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return UploadResult{}, ctx.Err()
	}
	// Preserve an early rejection even when it interrupted the TAR producer.
	if response != nil && response.StatusCode != http.StatusOK {
		_, err := readFileAcknowledgment(response)
		return UploadResult{}, err
	}
	if requestErr != nil || finished.err != nil || finished.bytes != info.Size() {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return UploadResult{}, errors.New("upload transfer failed; a partial new phone file may remain")
	}
	phase = "completion"
	data, err := readFileAcknowledgment(response)
	if err != nil {
		return UploadResult{}, err
	}
	result = UploadResult{FileName: name, Bytes: finished.bytes, LocalSHA256: finished.hash}
	if len(data) != 0 {
		var envelope map[string]json.RawMessage
		var details map[string]json.RawMessage
		_ = json.Unmarshal(data, &envelope)
		if json.Unmarshal(envelope["data"], &details) != nil || details["dropFileItems"] == nil {
			return result, nil
		}
		var reply struct {
			Data struct {
				Items []struct {
					FileName string `json:"fileName"`
					SavePath string `json:"savePath"`
				} `json:"dropFileItems"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &reply) != nil {
			return UploadResult{}, errors.New("invalid upload response")
		}
		if reply.Data.Items != nil {
			if len(reply.Data.Items) != 1 || reply.Data.Items[0].FileName != name || !validPhonePath(reply.Data.Items[0].SavePath) || path.Base(reply.Data.Items[0].SavePath) == "/" || path.Dir(reply.Data.Items[0].SavePath) != remoteDirectory {
				return UploadResult{}, errors.New("invalid phone upload destination")
			}
			result.SavePath = reply.Data.Items[0].SavePath
		}
	}
	return result, nil
}

func (p *PhoneClient) uploadRequest(ctx context.Context, route string, s *Session, body io.Reader, contentType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+route, body)
	if err != nil {
		return nil, errors.New("cannot build upload request")
	}
	req.Header.Set("newToken", s.Token)
	req.Header.Set("X-ES-HTTP-VERSION", "1")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connection", "keep-alive")
	return req, nil
}

func readFileAcknowledgment(response *http.Response) (json.RawMessage, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("missing upload response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == 445 || response.StatusCode == http.StatusForbidden {
			return nil, ErrFilePermission
		}
		return nil, &HTTPStatusError{Status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, errors.New("invalid or oversized upload response")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, nil
	}
	// Directory upload callers accept HTTP 200 alone (main.js 4529592,
	// 4471160); optional JSON is destination metadata, not an application status.
	return data, nil
}

func openUploadFile(localPath string) (*os.File, os.FileInfo, error) {
	parent, basename, err := openDownloadDirectory(localPath)
	if err != nil {
		return nil, nil, errors.New("invalid upload source")
	}
	defer parent.Close()
	fd, err := syscall.Openat(int(parent.Fd()), basename, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, errors.New("cannot open regular upload source")
	}
	file := os.NewFile(uintptr(fd), "upload-source")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxUploadBytes {
		file.Close()
		return nil, nil, errors.New("upload source must be a regular file of at most 1 GiB")
	}
	return file, info, nil
}

func uploadExtension(localPath string) string {
	extension := strings.ToLower(filepath.Ext(localPath))
	if len(extension) < 2 || len(extension) > 13 {
		return ""
	}
	for _, letter := range extension[1:] {
		if !(letter >= 'a' && letter <= 'z' || letter >= '0' && letter <= '9') {
			return ""
		}
	}
	return extension
}
