package vivo

import (
	"errors"
	"strings"
)

// Region selects which Office Kit build the PC impersonates during QR pairing.
//
// The vendor ships one Electron code base in two builds that differ only by
// build-config.json IS_EXPORT. Verified against pcsuite 7.0.6.0-cn
// (components-connection.js createQRCode/baseUrl, renderer.js
// GATEWAY_SERVER_ENUM, app-connection.js sendVersionRequest/checkVersion):
//   - export (global) builds pick a regional rendezvous gateway and rewrite
//     the QR scheme from https:// to pcsuite:, and send isOversea=true;
//   - the China-mainland build always uses https://psuite.vivo.com.cn, keeps
//     the QR as a plain https URL, and sends isOversea=false.
//
// The rendezvous RSA key, userId header, /scan endpoints and the phone version
// codes (pcSuite 70007, connBase 1155) are identical in both builds.
type Region struct {
	Name    string
	Gateway string
	// Domestic is true for the China-mainland (non-export) build.
	Domestic bool
}

const ChinaGateway = "https://psuite.vivo.com.cn/vbusiness"

var (
	RegionGlobal = Region{Name: "global", Gateway: IndiaGateway}
	RegionChina  = Region{Name: "cn", Gateway: ChinaGateway, Domestic: true}
)

func ParseRegion(value string) (Region, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "global", "export", "oversea", "in":
		return RegionGlobal, nil
	case "cn", "china":
		return RegionChina, nil
	}
	return Region{}, errors.New("region must be global or cn")
}
