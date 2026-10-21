package vivo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestGlobalMediaCategoryRequests(t *testing.T) {
	for _, tc := range []struct {
		category, kind string
		group          int
		categoryField  string
	}{
		{"recent", "REQUEST_POSTS_ONE_MOTH_LIST", 1, ""}, {"images", "REQUEST_POSTS_IMAGELIST", 1, ""}, {"videos", "REQUEST_POSTS_VIDEOLIST", 0, ""}, {"audio", "REQUEST_POSTS_AUDIOLIST", 0, ""}, {"documents", "REQUEST_POSTS_DOCSLIST", 5, ""},
	} {
		t.Run(tc.category, func(t *testing.T) {
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if r.URL.Path != "/pc_file_manager/channel" || r.Method != "POST" || r.Header.Get("newToken") != "secret" {
					t.Error("wrong authenticated route")
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid JSON")
				}
				wantData := ""
				if tc.category == "documents" {
					wantData = "[PDF, DOC, XLS, PPT, TXT, VCF,IWORK,CAD, PSD, XMIND, 其他]"
				}
				if body["type"] != tc.kind || body["category"] != tc.categoryField || body["groupBy"] != float64(tc.group) || body["data"] != wantData || body["sortCondition"] != float64(5) || body["pageIndex"] != float64(3) || body["pageNumber"] != float64(200) || body["fileCount"] != float64(0) {
					t.Error("category contract mismatch")
				}
				if _, ok := body["versionTag"]; ok {
					t.Error("directory-only versionTag leaked into global collection")
				}
				fmt.Fprint(w, `{"dataList":[],"totalCount":0}`)
			})
			page, err := client.ListMediaCategory(context.Background(), &Session{Token: "secret"}, tc.category, 3)
			if err != nil || len(page.DataList) != 0 {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizeGlobalMediaShapes(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		recent       bool
		group, stamp string
	}{
		{`[{"isGroup":true,"groupCount":1,"groupName":"Today"},{"fileName":"fixture.jpg","savePath":"/storage/fixture.jpg","fileSize":3,"date":123}]`, false, "Today", ""},
		{`[{"time":"Today","dataList":{"Camera | Images":[{"fileName":"fixture.jpg","savePath":"/storage/fixture.jpg","fileSize":3,"date":123}]}}]`, true, "Camera | Images", "Today"},
	} {
		entries, err := normalizeMediaEntries([]byte(tc.raw), tc.recent)
		if err != nil || len(entries) != 1 || entries[0].GroupName != tc.group || entries[0].GroupTime != tc.stamp || entries[0].Date != 123 {
			t.Fatal("grouped media parsing failed", err)
		}
	}
	for _, raw := range []string{`null`, `{}`, `[{"fileName":"fixture","savePath":"/storage/../fixture","fileSize":1}]`, `[{"fileName":"mismatch","savePath":"/storage/fixture","fileSize":1}]`, `[{"time":"Today","dataList":{"Images":[{"fileName":"../fixture","savePath":"/storage/fixture","fileSize":1}]}}]`, `[{"time":{},"dataList":{"Images":[]}}]`} {
		if _, err := normalizeMediaEntries([]byte(raw), true); err == nil {
			t.Fatal("unsafe media list accepted")
		}
	}
}
