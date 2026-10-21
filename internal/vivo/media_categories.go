package vivo

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

func ValidMediaCategory(category string) bool {
	switch category {
	case "recent", "images", "videos", "audio", "documents":
		return true
	}
	return false
}

// Shipped app-modules.js module136, offsets67173–71110: these global
// collections use distinct request types, not a filtered directory listing.
func (p *PhoneClient) ListMediaCategory(ctx context.Context, s *Session, category string, page int) (FilePage, error) {
	if s == nil || s.Token == "" || !ValidMediaCategory(category) || page < 0 {
		return FilePage{}, errors.New("invalid media category request")
	}
	types := map[string]string{"recent": "REQUEST_POSTS_ONE_MOTH_LIST", "images": "REQUEST_POSTS_IMAGELIST", "videos": "REQUEST_POSTS_VIDEOLIST", "audio": "REQUEST_POSTS_AUDIOLIST", "documents": "REQUEST_POSTS_DOCSLIST"}
	group := 0
	data := ""
	cat := ""
	if category == "recent" || category == "images" {
		group = 1
	}
	if category == "documents" {
		group = 5
		data = "[PDF, DOC, XLS, PPT, TXT, VCF,IWORK,CAD, PSD, XMIND, 其他]"
	}
	body := map[string]any{"category": cat, "data": data, "fileCount": 0, "sortCondition": 5, "groupBy": group, "type": types[category], "pageIndex": page, "pageNumber": 200}
	var response struct {
		DataList   json.RawMessage `json:"dataList"`
		TotalCount int64           `json:"totalCount"`
	}
	if err := p.post(ctx, "/pc_file_manager/channel", s, body, 30*time.Second, &response); err != nil {
		return FilePage{}, err
	}
	if response.TotalCount < 0 {
		return FilePage{}, errors.New("invalid media count")
	}
	entries, err := normalizeMediaEntries(response.DataList, category == "recent")
	if err != nil {
		return FilePage{}, err
	}
	return FilePage{DataList: entries, TotalCount: response.TotalCount}, nil
}

func normalizeMediaEntries(raw json.RawMessage, recent bool) ([]FileEntry, error) {
	var values []json.RawMessage
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &values) != nil || len(values) > 2000 {
		return nil, errors.New("invalid media list")
	}
	result := make([]FileEntry, 0, len(values))
	groupName, groupTime := "", ""
	appendFile := func(raw json.RawMessage, name, stamp string) error {
		var entry FileEntry
		if json.Unmarshal(raw, &entry) != nil || !validFileEntry(entry) || entry.IsGroup {
			return errors.New("invalid media entry")
		}
		if len(result) >= 2000 {
			return errors.New("media list exceeds limit")
		}
		entry.GroupName = name
		entry.GroupTime = stamp
		result = append(result, entry)
		return nil
	}
	for _, value := range values {
		var shape struct {
			IsGroup   bool            `json:"isGroup"`
			GroupName string          `json:"groupName"`
			Time      json.RawMessage `json:"time"`
			DataList  json.RawMessage `json:"dataList"`
		}
		if json.Unmarshal(value, &shape) != nil {
			return nil, errors.New("invalid media entry")
		}
		if shape.IsGroup {
			if len(shape.GroupName) > 256 {
				return nil, errors.New("invalid media group")
			}
			groupName = shape.GroupName
			continue
		}
		if recent && len(shape.DataList) > 0 {
			var groups map[string][]json.RawMessage
			if json.Unmarshal(shape.DataList, &groups) != nil || len(groups) > 200 {
				return nil, errors.New("invalid recent media group")
			}
			stamp := ""
			if len(shape.Time) > 0 {
				if json.Unmarshal(shape.Time, &stamp) != nil {
					var number json.Number
					if json.Unmarshal(shape.Time, &number) != nil {
						return nil, errors.New("invalid recent time")
					}
					stamp = number.String()
				}
				if len(stamp) > 256 {
					return nil, errors.New("invalid recent time")
				}
			}
			names := make([]string, 0, len(groups))
			for name := range groups {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if len(name) > 256 {
					return nil, errors.New("invalid recent group")
				}
				for _, file := range groups[name] {
					if err := appendFile(file, name, stamp); err != nil {
						return nil, err
					}
				}
			}
		} else {
			if err := appendFile(value, groupName, groupTime); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
