package vivo

import "errors"

// Region selects the phone edition's QR handler, rendezvous and handshake.
// The zero value preserves the original overseas client behavior.
type Region string

const (
	RegionOverseas Region = "overseas"
	RegionChina    Region = "china"
	ChinaGateway          = "https://psuite.vivo.com.cn/vbusiness"
)

func ParseRegion(value string) (Region, error) {
	switch Region(value) {
	case RegionOverseas, RegionChina:
		return Region(value), nil
	default:
		return "", errors.New("region must be overseas or china")
	}
}

func (r Region) Overseas() bool { return r != RegionChina }

func (r Region) Gateway() string {
	if r == RegionChina {
		return ChinaGateway
	}
	return IndiaGateway
}

func (r Region) qrURI(httpsURL string) string {
	if r == RegionChina {
		return httpsURL
	}
	return "pcsuite:" + httpsURL[len("https://"):]
}
