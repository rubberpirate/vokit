package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"vivolinux/internal/desktop"
	"vivolinux/internal/service"
	"vivolinux/internal/vivo"
)

type pairOptions struct {
	stateDir, pin, localIP    string
	phone                     netip.Addr
	duration                  time.Duration
	trustScanned              bool
	clipboard, notifications  bool
	helper                    string
	listDir, download, output string
	filePage                  int
	nativeSocket              string
	fileAccess, mirror        bool
	persistent                bool
}

func privateAddress(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(value)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, errors.New("phone must be a literal private LAN IP address without an interface zone")
	}
	address = address.Unmap()
	if err != nil || !address.IsPrivate() || address.Zone() != "" {
		return netip.Addr{}, errors.New("phone must be a literal private LAN IP address")
	}
	return address, nil
}

func parsePairOptions(args []string) (pairOptions, error) {
	var options pairOptions
	var phone string
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	flags.StringVar(&options.stateDir, "state-dir", "build/pairing", "private pairing state directory")
	flags.StringVar(&options.pin, "phone-cert-sha256", "", "verified phone certificate SHA-256 fingerprint")
	flags.BoolVar(&options.trustScanned, "trust-scanned-phone", false, "explicitly trust first certificate from --phone after this scan; pin it for this session")
	flags.StringVar(&phone, "phone", "", "only connect to this phone LAN IP from the scan result")
	flags.StringVar(&options.localIP, "local-ip", "", "local LAN IP (otherwise automatically selected)")
	flags.DurationVar(&options.duration, "duration", 5*time.Minute, "control test duration after negotiation")
	flags.BoolVar(&options.clipboard, "clipboard", false, "sync new plain-text clipboard changes while connected")
	flags.BoolVar(&options.notifications, "notifications", false, "show local connection status notifications")
	flags.StringVar(&options.helper, "desktop-helper", "", "native desktop helper path (default beside executable)")
	flags.StringVar(&options.listDir, "list-files", "", "request permission and list one phone directory into private files.json")
	flags.StringVar(&options.download, "download", "", "download this exact phone file after permission and listing")
	flags.StringVar(&options.output, "output", "", "new local destination for --download; never overwritten")
	flags.IntVar(&options.filePage, "file-page", 0, "zero-based file listing page, 200 entries per page")
	flags.StringVar(&options.nativeSocket, "native-socket", service.DefaultNativeSocket(), "private socket for native-status/files/download while connected")
	flags.BoolVar(&options.fileAccess, "file-access", false, "request phone file access for commands on the active session")
	flags.BoolVar(&options.mirror, "mirror-native", false, "experimental native video-only H.264 preview using ffplay")
	flags.BoolVar(&options.persistent, "persistent", false, "keep the authorized session running until stopped or disconnected")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("pair takes flags only")
	}
	if options.duration <= 0 || options.duration > 24*time.Hour {
		return options, errors.New("duration must be greater than zero and at most 24h")
	}
	if options.pin != "" {
		if _, err := vivo.ParseCertificatePin(options.pin); err != nil {
			return options, err
		}
	}
	if phone != "" {
		var err error
		options.phone, err = privateAddress(phone)
		if err != nil {
			return options, err
		}
	}
	if options.localIP != "" {
		if _, err := privateAddress(options.localIP); err != nil {
			return options, errors.New("local-ip must be a private LAN address")
		}
	}
	if options.trustScanned && (!options.phone.IsValid() || options.pin != "") {
		return options, errors.New("trust-scanned-phone requires --phone and cannot be combined with an existing pin")
	}
	if (options.download == "") != (options.output == "") || options.filePage < 0 {
		return options, errors.New("download requires both --download and --output; file-page must be nonnegative")
	}
	if options.download != "" && options.listDir == "" {
		options.listDir = path.Dir(options.download)
	}
	for _, value := range []string{options.listDir, options.download} {
		if value != "" && (!strings.HasPrefix(value, "/") || path.Clean(value) != value || strings.ContainsAny(value, "\x00\\")) {
			return options, errors.New("phone file paths must be normalized absolute paths")
		}
	}
	return options, nil
}

func localLANIP() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", errors.New("cannot enumerate LAN interfaces; specify --local-ip")
	}
	for _, device := range interfaces {
		if device.Flags&net.FlagUp == 0 || device.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := device.Addrs()
		for _, value := range addresses {
			prefix, err := netip.ParsePrefix(value.String())
			if err == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() {
				return prefix.Addr().String(), nil
			}
		}
	}
	return "", errors.New("no private IPv4 LAN address found; specify --local-ip")
}

// The URI is secret input on stdin, never a process argument or diagnostic string.
func writeQR(ctx context.Context, uri, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot create private QR image")
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	cmd := exec.CommandContext(ctx, "qrencode", "-t", "PNG", "-s", "6", "-o", "-")
	cmd.Stdin = strings.NewReader(uri)
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		return errors.New("QR image generation failed; install qrencode")
	}
	if err := f.Sync(); err != nil {
		return errors.New("cannot persist QR image")
	}
	if err := f.Close(); err != nil {
		return errors.New("cannot close QR image")
	}
	complete = true
	return nil
}

func runPair(ctx context.Context, args []string) error {
	options, err := parsePairOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("qrencode"); err != nil {
		return errors.New("install qrencode before starting native QR pairing")
	}
	if options.mirror {
		if _, err := exec.LookPath("ffplay"); err != nil {
			return errors.New("install ffplay for native video preview")
		}
	}
	var bridge *desktop.Bridge
	if options.clipboard || options.notifications {
		if options.helper == "" {
			executable, err := os.Executable()
			if err != nil {
				return errors.New("cannot find desktop helper path")
			}
			options.helper = filepath.Join(filepath.Dir(executable), "vivo-desktop-helper")
		}
		bridge, err = desktop.Start(ctx, options.helper, options.clipboard)
		if err != nil {
			return err
		}
		defer bridge.Close()
	}
	if options.localIP == "" {
		options.localIP, err = localLANIP()
		if err != nil {
			return err
		}
	}
	id, err := vivo.LoadIdentity(options.stateDir)
	if err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return errors.New("cannot determine PC name")
	}
	session, err := vivo.NewSession(id, hostname, options.localIP)
	if err != nil {
		return err
	}
	cloud, err := vivo.NewRendezvous(vivo.IndiaGateway, id, nil)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(options.stateDir, "scan-")
	if err != nil {
		return errors.New("cannot create private test-session directory")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return errors.New("cannot resolve test-session directory")
	}
	log, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot create private event log")
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	encoder := json.NewEncoder(io.MultiWriter(os.Stdout, log))
	var logErr error
	emit := func(event vivo.Event) {
		if logErr == nil {
			logErr = encoder.Encode(event)
			if logErr != nil {
				cancel()
			}
		}
	}
	stage := func(name string) { emit(vivo.Event{Time: time.Now().UTC(), Stage: name}) }
	fmt.Fprintln(os.Stderr, "Native QR pairing test. Event log:", filepath.Join(dir, "events.jsonl"))
	if err := cloud.Begin(ctx, session); err != nil {
		return err
	}
	uri, err := session.QR(vivo.IndiaGateway)
	if err != nil {
		return err
	}
	qrPath := filepath.Join(dir, "qr.png")
	if err := writeQR(ctx, uri, qrPath); err != nil {
		return err
	}
	defer os.Remove(qrPath) // Pairing tokens are not retained after this attempt.
	fmt.Fprintln(os.Stderr, "Scan this image using the phone's Office Kit scanner:", qrPath)
	fmt.Fprintln(os.Stderr, "QR expires:", session.ExpiresAt.Format(time.RFC3339))
	emit(vivo.Event{Time: time.Now().UTC(), Stage: "qr_ready", QRPath: qrPath})
	peer, err := cloud.Wait(ctx, session)
	if err != nil {
		return err
	}
	if peer.ConnectionID != "" {
		session.ConnectionID = peer.ConnectionID
	}
	stage("scan_received")
	if peer.IgnoredAddresses > 0 {
		emit(vivo.Event{Time: time.Now().UTC(), Stage: "non_lan_addresses_ignored", Count: peer.IgnoredAddresses})
	}
	var client *vivo.PhoneClient
	var capabilities map[string]bool
	var phoneAddress netip.Addr
	var lastErr error
	for _, address := range peer.Addresses {
		if options.phone.IsValid() && address != options.phone {
			continue
		}
		pin := options.pin
		var candidate *vivo.PhoneClient
		if options.trustScanned {
			candidate, err = vivo.NewFirstUsePhoneClient(address)
		} else {
			candidate, err = vivo.NewPhoneClient(address, pin)
		}
		if err != nil {
			return err
		}
		capabilities, err = candidate.Negotiate(ctx, session)
		if options.trustScanned && candidate.CertificatePin() != "" {
			fmt.Fprintln(os.Stderr, "Explicit first-use trust: pinned actual phone connection certificate:", candidate.CertificatePin())
			stage("scanned_phone_certificate_pinned")
		}
		if err == nil {
			client = candidate
			phoneAddress = address
			break
		}
		candidate.Close()
		if options.trustScanned {
			return err
		} // Never refresh first-use trust after failure.
		lastErr = err
		if pin == "" {
			fingerprint, inspectErr := vivo.InspectCertificate(ctx, address)
			if inspectErr == nil {
				fmt.Fprintln(os.Stderr, "Unverified phone certificate SHA-256:", fingerprint)
				fmt.Fprintln(os.Stderr, "Verify this belongs to your phone before retrying with --phone-cert-sha256 and a fresh QR.")
			}
		}
	}
	if client == nil {
		if lastErr != nil {
			return lastErr
		}
		return errors.New("scan result did not include the requested phone IP")
	}
	defer client.Close()
	emit(vivo.Event{Time: time.Now().UTC(), Stage: "phone_negotiated", Capabilities: capabilities})
	var testCtx context.Context
	var stop context.CancelFunc
	if options.persistent {
		testCtx, stop = context.WithCancel(ctx)
	} else {
		testCtx, stop = context.WithTimeout(ctx, options.duration)
	}
	defer stop()
	features := vivo.ControlOptions{}
	var connected, fileAllowed atomic.Bool
	media := newNativeMedia(session, phoneAddress, options.trustScanned, options.pin, connected.Load, func(e vivo.Event) { emit(e) })
	features.MainRequests = media.requests
	features.MirrorPermissionState = media.mirrorPermission
	features.CameraMessage = media.cameraMessage
	var fileAccess vivo.FileAccess
	fileRequests := make(chan struct{}, 1)
	if (options.fileAccess || options.listDir != "") && capabilities["hasStoryPermission"] {
		fileAccess.Update(true)
		fileAllowed.Store(true)
	}
	var fileWorker sync.WaitGroup
	var eventMutex sync.Mutex
	// File jobs run concurrently with control reads/heartbeats; serialize logs.
	oldEmit := emit
	emit = func(event vivo.Event) { eventMutex.Lock(); defer eventMutex.Unlock(); oldEmit(event) }
	if options.clipboard {
		features.Clipboard = bridge
	}
	features.Connected = func() {
		connected.Store(true)
		if options.notifications {
			if bridge.Notify(testCtx, "connected") != nil {
				stage("notification_unavailable")
			}
		}
	}
	features.FilePermissionRequests = fileRequests
	features.FilePermissionState = func(allowed bool) { fileAccess.Update(allowed); fileAllowed.Store(allowed) }
	if options.fileAccess && options.listDir == "" {
		features.FilePermission = make(chan bool, 1)
	}
	if options.listDir != "" {
		permissions := make(chan bool, 1)
		features.FilePermission = permissions
		fileWorker.Add(1)
		go func() {
			defer fileWorker.Done()
			timer := time.NewTimer(60 * time.Second)
			defer timer.Stop()
			select {
			case <-testCtx.Done():
				return
			case <-timer.C:
				stage("file_permission_timeout")
				return
			case allowed := <-permissions:
				if !allowed {
					stage("file_permission_denied")
					return
				}
			}
			stage("file_permission_granted")
			var dataClient *vivo.PhoneClient
			var err error
			if options.trustScanned {
				dataClient, err = vivo.NewFirstUsePhoneClient(phoneAddress)
			} else {
				dataClient, err = vivo.NewPhoneClient(phoneAddress, options.pin)
			}
			if err != nil {
				stage("file_channel_failed")
				return
			}
			defer dataClient.Close()
			page, err := dataClient.ListFiles(testCtx, session, options.listDir, options.filePage)
			if err != nil {
				stage("file_listing_failed")
				return
			}
			if err := writePrivateJSON(filepath.Join(dir, "files.json"), page); err != nil {
				stage("file_listing_save_failed")
				return
			}
			emit(vivo.Event{Time: time.Now().UTC(), Stage: "file_listing_completed", Count: len(page.DataList)})
			if options.download == "" {
				return
			}
			for _, entry := range page.DataList {
				if entry.SavePath != options.download {
					continue
				}
				result, err := dataClient.DownloadFile(testCtx, session, entry, options.output)
				if err != nil {
					stage("file_download_failed")
					return
				}
				if writePrivateJSON(filepath.Join(dir, "download.json"), result) != nil {
					stage("file_download_receipt_failed")
					return
				}
				stage("file_download_completed")
				return
			}
			stage("file_selection_not_in_page")
		}()
	}
	fileWorker.Add(1)
	go func() {
		defer fileWorker.Done()
		err := service.ServeNative(testCtx, options.nativeSocket, func(requestCtx context.Context, request service.NativeRequest) (any, error) {
			if request.Method == "status" {
				return map[string]any{"backend": "native-vivo", "connected": connected.Load(), "clipboard_enabled": options.clipboard, "file_permission": fileAllowed.Load()}, nil
			}
			if !connected.Load() {
				return nil, errors.New("phone is disconnected")
			}
			if err := fileAccess.Ensure(requestCtx, fileRequests, false); err != nil {
				stage("file_permission_not_granted")
				return nil, service.ErrNativeFilePermission
			}
			var dataClient *vivo.PhoneClient
			var err error
			if options.trustScanned {
				dataClient, err = vivo.NewFirstUsePhoneClient(phoneAddress)
			} else {
				dataClient, err = vivo.NewPhoneClient(phoneAddress, options.pin)
			}
			if err != nil {
				return nil, err
			}
			defer dataClient.Close()
			if request.Method == "upload" {
				result, err := dataClient.UploadFile(requestCtx, session, request.Local, request.Directory)
				if err != nil {
					code := vivo.FileErrorCode(err)
					emit(vivo.Event{Time: time.Now().UTC(), Stage: "file_upload_failed", Code: code})
					if errors.Is(err, vivo.ErrFilePermission) {
						fileAccess.Update(false)
						fileAllowed.Store(false)
						return nil, service.ErrNativeFilePermission
					}
				} else {
					stage("file_upload_completed")
				}
				return result, err
			}
			directory := request.Directory
			if request.Method == "download" || request.Method == "thumbnail" || request.Method == "preview" {
				directory = path.Dir(request.Remote)
			}
			page, err := dataClient.ListFiles(requestCtx, session, directory, request.Page)
			if err != nil {
				stage("file_listing_failed")
				if errors.Is(err, vivo.ErrFilePermission) {
					fileAccess.Update(false)
					fileAllowed.Store(false)
					return nil, service.ErrNativeFilePermission
				}
				return nil, err
			}
			if request.Method == "files" {
				return page, nil
			}
			for _, entry := range page.DataList {
				if entry.SavePath == request.Remote {
					var result any
					var err error
					switch request.Method {
					case "thumbnail":
						result, err = dataClient.FetchThumbnail(requestCtx, session, entry, request.Width, request.Height)
					case "preview":
						result, err = dataClient.PrepareMediaPreview(requestCtx, session, entry, request.Output)
					default:
						result, err = dataClient.DownloadFile(requestCtx, session, entry, request.Output)
					}
					if err != nil {
						stage("file_download_failed")
						if errors.Is(err, vivo.ErrFilePermission) {
							fileAccess.Update(false)
							fileAllowed.Store(false)
							return nil, service.ErrNativeFilePermission
						}
					} else {
						stage("file_download_completed")
					}
					return result, err
				}
			}
			return nil, errors.New("selected file not in page")
		})
		if err != nil && testCtx.Err() == nil {
			stage("native_rpc_unavailable")
		}
	}()
	fmt.Fprintln(os.Stderr, "Native session socket:", options.nativeSocket)
	if options.mirror {
		legacyCtx, legacyStop := context.WithCancel(testCtx)
		legacyDone := make(chan struct{})
		media.stopLegacy, media.legacyDone = legacyStop, legacyDone
		defer legacyStop()
		permissions := make(chan bool, 1)
		features.MirrorPermission = permissions
		fileWorker.Add(1)
		go func() {
			defer fileWorker.Done()
			defer close(legacyDone)
			timer := time.NewTimer(60 * time.Second)
			defer timer.Stop()
			select {
			case <-legacyCtx.Done():
				return
			case <-timer.C:
				stage("mirror_permission_timeout")
				return
			case allowed := <-permissions:
				if !allowed {
					stage("mirror_permission_denied")
					return
				}
			}
			stage("mirror_permission_granted")
			if err := runNativeMirror(legacyCtx, session, phoneAddress, options.trustScanned, options.pin, emit); err != nil && legacyCtx.Err() == nil {
				stage("mirror_preview_stopped")
			}
		}()
	}
	fileWorker.Add(1)
	go func() {
		defer fileWorker.Done()
		if err := service.ServeMedia(testCtx, filepath.Join(filepath.Dir(options.nativeSocket), "media.sock"), media.serve); err != nil && testCtx.Err() == nil {
			stage("media_service_unavailable")
		}
	}()
	err = client.ControlWithFeatures(testCtx, session, features, emit)
	connected.Store(false)
	stop()
	fileWorker.Wait()
	if options.notifications {
		notifyCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
		_ = bridge.Notify(notifyCtx, "disconnected")
		done()
	}
	if logErr != nil {
		return errors.New("cannot write sanitized test events")
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		stage("test_duration_completed")
		return logErr
	}
	return err
}

func writePrivateJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot create private result file")
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return errors.New("cannot write private result file")
	}
	return f.Sync()
}

func runPhoneCertificate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("phone-certificate", flag.ContinueOnError)
	var value string
	flags.StringVar(&value, "phone", "", "phone's private LAN IP")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("phone-certificate takes flags only")
	}
	address, err := privateAddress(value)
	if err != nil {
		return err
	}
	fingerprint, err := vivo.InspectCertificate(ctx, address)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"sha256": fingerprint, "authenticated": false, "note": "Public certificate metadata only. Verify ownership before using as a pairing pin."})
}
