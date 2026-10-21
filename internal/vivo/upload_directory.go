package vivo

import (
	"context"
	"errors"
	"time"
)

// The UI's browsing root is not a phone write path. Shipped app-modules.js
// 169600–170300 reads HTTP response body.result_path without an envelope/code.
func (p *PhoneClient) ResolveUploadDirectory(ctx context.Context, s *Session, directory string) (string, error) {
	if s == nil || s.Token == "" || !validPhonePath(directory) {
		return "", errors.New("invalid upload directory")
	}
	if directory != "/" {
		return directory, nil
	}
	// Storage browsing root maps to base-info.innerRoot in the vendor UI.
	if s.StorageRoot != "/" && validPhonePath(s.StorageRoot) {
		return s.StorageRoot, nil
	}
	var reply struct {
		ResultPath string `json:"result_path"`
	}
	if err := p.post(ctx, "/pc_file_manager/get_path", s, map[string]string{"id": "1313", "path_type": ""}, 30*time.Second, &reply); err != nil {
		return "", err
	}
	if reply.ResultPath == "/" || !validPhonePath(reply.ResultPath) {
		return "", errors.New("phone returned an invalid upload directory")
	}
	return reply.ResultPath, nil
}
