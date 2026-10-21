package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"vivolinux/internal/service"
)

func runNative(ctx context.Context, args []string) error {
	socket, request, err := parseNativeOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if request.Method == "download" || request.Method == "preview" {
		if _, err := os.Lstat(request.Output); !errors.Is(err, os.ErrNotExist) {
			return errors.New("download destination already exists or is unavailable")
		}
	}
	if request.Method == "upload" {
		info, err := os.Lstat(request.Local)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<30 {
			return errors.New("upload source must be a regular file of at most 1 GiB")
		}
	}
	result, err := service.CallNative(ctx, socket, request)
	if err != nil {
		if errors.Is(err, service.ErrNativeFilePermission) {
			_ = printJSON(map[string]string{"error": "file_permission_required"})
		}
		return err
	}
	return printJSON(result)
}

func parseNativeOptions(args []string) (string, service.NativeRequest, error) {
	request := service.NativeRequest{}
	if len(args) == 0 {
		return "", request, errors.New("native command is required")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := service.DefaultNativeSocket()
	flags.StringVar(&socket, "socket", socket, "private native session socket")
	switch args[0] {
	case "native-status":
		request.Method = "status"
	case "files":
		request.Method = "files"
		flags.StringVar(&request.Directory, "path", "/", "absolute phone directory")
		flags.IntVar(&request.Page, "page", 0, "directory page")
	case "download", "preview":
		request.Method = args[0]
		flags.StringVar(&request.Remote, "remote", "", "absolute selected phone file")
		flags.StringVar(&request.Output, "output", "", "new local destination file")
		flags.IntVar(&request.Page, "page", 0, "page containing the selected file")
	case "thumbnail":
		request.Method = "thumbnail"
		flags.StringVar(&request.Remote, "remote", "", "absolute selected phone media file")
		flags.IntVar(&request.Page, "page", 0, "page containing selected media")
		flags.IntVar(&request.Width, "width", 128, "thumbnail width")
		flags.IntVar(&request.Height, "height", 128, "thumbnail height")
	case "upload":
		request.Method = "upload"
		flags.StringVar(&request.Local, "local", "", "explicit regular local file")
		flags.StringVar(&request.Directory, "to", "", "absolute phone destination directory")
	default:
		return "", request, errors.New("unsupported native command")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", request, flag.ErrHelp
		}
		return "", request, errors.New("invalid native command options")
	}
	validPath := func(value string) bool {
		return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsAny(value, "\x00\\")
	}
	if flags.NArg() != 0 || !validPath(socket) || request.Page < 0 {
		return "", request, errors.New("invalid native command options")
	}
	if request.Method == "files" && !validPath(request.Directory) {
		return "", request, errors.New("phone directory must be a normalized absolute path")
	}
	if request.Method == "thumbnail" && (!validPath(request.Remote) || request.Width < 1 || request.Width > 1024 || request.Height < 1 || request.Height > 1024) {
		return "", request, errors.New("invalid thumbnail selection or size")
	}
	if request.Method == "download" || request.Method == "preview" {
		if !validPath(request.Remote) || request.Output == "" || strings.ContainsAny(request.Output, "\x00\\") || strings.HasSuffix(request.Output, "/") {
			return "", request, errors.New("download requires an absolute remote path and a new output file")
		}
		for _, part := range strings.Split(request.Output, "/") {
			if part == ".." {
				return "", request, errors.New("invalid download destination")
			}
		}
		output, err := filepath.Abs(request.Output)
		if err != nil {
			return "", request, errors.New("invalid download destination")
		}
		request.Output = output
	}
	if request.Method == "upload" {
		if request.Local == "" || strings.ContainsAny(request.Local, "\x00\\") || !validPath(request.Directory) {
			return "", request, errors.New("upload requires a regular local file and an absolute phone directory")
		}
		for _, part := range strings.Split(request.Local, "/") {
			if part == ".." {
				return "", request, errors.New("invalid upload source")
			}
		}
		local, err := filepath.Abs(request.Local)
		if err != nil {
			return "", request, errors.New("invalid upload source")
		}
		request.Local = local
	}
	return socket, request, nil
}
