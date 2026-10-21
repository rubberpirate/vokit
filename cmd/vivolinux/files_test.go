package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vivolinux/internal/service"
	"vivolinux/internal/vivo"
)

func TestNativeCLIValidation(t *testing.T) {
	for _, args := range [][]string{
		{}, {"other"}, {"files", "extra"}, {"files", "--path", "relative"},
		{"files", "--path", "/storage/../private"}, {"files", "--path", "/storage//file"},
		{"files", "--page", "-1"}, {"files", "--socket", "relative"},
		{"native-status", "--remote", "/file"}, {"download"},
		{"download", "--remote", "relative", "--output", "new"},
		{"download", "--remote", "/file", "--output", "../new"},
		{"download", "--remote", "/file", "--output", "new/"},
		{"upload"}, {"upload", "--local", "source", "--to", "relative"},
		{"upload", "--local", "../source", "--to", "/storage"},
		{"upload", "--local", "source", "--to", "/storage/../private"},
		{"thumbnail", "--remote", "relative"}, {"thumbnail", "--remote", "/file", "--width", "1025"},
		{"preview", "--remote", "/file"}, {"preview", "--remote", "/file", "--output", "../new"},
	} {
		if _, _, err := parseNativeOptions(args); err == nil {
			t.Fatalf("accepted invalid native options %#v", args)
		}
	}
	socket, req, err := parseNativeOptions([]string{"files", "--path", "/storage/emulated/0", "--page", "2"})
	if err != nil || socket != service.DefaultNativeSocket() || req.Method != "files" || req.Directory != "/storage/emulated/0" || req.Page != 2 {
		t.Fatal("valid listing options rejected", err)
	}
	_, req, err = parseNativeOptions([]string{"download", "--remote", "/storage/emulated/0/file", "--output", "new-file", "--page", "3"})
	if err != nil || req.Method != "download" || !filepath.IsAbs(req.Output) || req.Page != 3 {
		t.Fatal("valid download options rejected", err)
	}
	_, req, err = parseNativeOptions([]string{"native-status"})
	if err != nil || req.Method != "status" {
		t.Fatal("native status rejected", err)
	}
}

func TestMediaCLISelectionAndCachePaths(t *testing.T) {
	_, request, err := parseNativeOptions([]string{"thumbnail", "--remote", "/storage/picture.jpg", "--page", "2"})
	if err != nil || request.Method != "thumbnail" || request.Width != 128 || request.Height != 128 || request.Page != 2 {
		t.Fatal("invalid thumbnail request", err)
	}
	_, request, err = parseNativeOptions([]string{"preview", "--remote", "/storage/clip.mp4", "--output", "private-cache", "--page", "3"})
	if err != nil || request.Method != "preview" || !filepath.IsAbs(request.Output) || request.Page != 3 {
		t.Fatal("invalid preview request", err)
	}
}

func TestUploadCLIOptionsAndSourceValidation(t *testing.T) {
	_, req, err := parseNativeOptions([]string{"upload", "--local", "source", "--to", "/storage/emulated/0"})
	if err != nil || req.Method != "upload" || !filepath.IsAbs(req.Local) || req.Directory != "/storage/emulated/0" {
		t.Fatal("valid upload options rejected", err)
	}
	directory := t.TempDir()
	file := filepath.Join(directory, "source")
	os.WriteFile(file, []byte("fixture"), 0600)
	link := filepath.Join(directory, "link")
	os.Symlink(file, link)
	for _, local := range []string{link, directory, filepath.Join(directory, "missing")} {
		err := runNative(context.Background(), []string{"upload", "--local", local, "--to", "/storage", "--socket", filepath.Join(directory, "missing.sock")})
		if err == nil || !strings.Contains(err.Error(), "regular file") || strings.Contains(err.Error(), local) {
			t.Fatal("unsafe upload CLI source validation", err)
		}
	}
}

func TestNativeCLIRefusesOverwriteBeforeRPC(t *testing.T) {
	file := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	err := runNative(context.Background(), []string{"download", "--socket", filepath.Join(t.TempDir(), "missing.sock"), "--remote", "/fixture", "--output", file})
	if err == nil || !strings.Contains(err.Error(), "destination") || strings.Contains(err.Error(), file) {
		t.Fatal("unsafe overwrite validation", err)
	}
	if data, _ := os.ReadFile(file); string(data) != "original" {
		t.Fatal("existing output was changed")
	}
}

func TestNativeCategoryCLIAndSelectionScope(t *testing.T) {
	for _, method := range []string{"files", "thumbnail", "preview", "download"} {
		args := []string{method, "--category", "images", "--page", "7"}
		if method != "files" {
			args = append(args, "--remote", "/storage/fixture.jpg")
		}
		if method == "download" || method == "preview" {
			args = append(args, "--output", filepath.Join(t.TempDir(), "new"))
		}
		_, request, err := parseNativeOptions(args)
		if err != nil || request.Category != "images" || request.Page != 7 || request.Directory != "" {
			t.Fatal("global category CLI failed", err)
		}
		lister := &categoryLister{}
		page, err := nativeRequestPage(context.Background(), lister, &vivo.Session{}, request)
		if err != nil || !lister.usedCategory || lister.category != "images" || lister.page != 7 || len(page.DataList) != 1 {
			t.Fatal("selection relisted wrong scope", err)
		}
	}
	for _, args := range [][]string{{"files", "--category", "images", "--path", "/"}, {"files", "--category", "invalid"}, {"upload", "--category", "images"}, {"native-status", "--category", "images"}} {
		if _, _, err := parseNativeOptions(args); err == nil {
			t.Fatal("invalid collection request accepted")
		}
	}
}

type categoryLister struct {
	usedCategory bool
	category     string
	page         int
}

func (l *categoryLister) ListFiles(context.Context, *vivo.Session, string, int) (vivo.FilePage, error) {
	return vivo.FilePage{}, nil
}
func (l *categoryLister) ListMediaCategory(_ context.Context, _ *vivo.Session, category string, page int) (vivo.FilePage, error) {
	l.usedCategory = true
	l.category = category
	l.page = page
	return vivo.FilePage{DataList: []vivo.FileEntry{{FileName: "fixture.jpg", SavePath: "/storage/fixture.jpg", FileSize: 3}}}, nil
}
