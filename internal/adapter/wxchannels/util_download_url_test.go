package wxchannelsadapter

import (
	"encoding/json"
	"testing"

	"wx_channel/pkg/scraper/wxchannels"
)

func downloadURLObject(t *testing.T, baseURL, token string) *wxchannels.ChannelsObject {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"objectDesc": map[string]any{
			"mediaType": 4,
			"media": []any{map[string]any{
				"url": baseURL, "urlToken": token,
				"spec": []any{map[string]string{"fileFormat": "xWT111"}, map[string]string{"fileFormat": "xWT113"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var obj wxchannels.ChannelsObject
	if err := json.Unmarshal(payload, &obj); err != nil {
		t.Fatal(err)
	}
	return &obj
}

func TestDownloadURLOriginalRetainsSignedParametersAndDoesNotSelectHighest(t *testing.T) {
	const base = "https://finder.video.qq.com/251/20302/stodownload?bizid=1023&dotrans=0&encfilekey=file"
	const token = "&token=a%2Fb%3D&basedata=data&sign=a+b%2f&web=1&svrbypass=c%26d&svrnonce=123"
	obj := downloadURLObject(t, base, token)
	for _, spec := range []string{"", "original"} {
		if got, want := BuildDownloadURLWithSpec(obj, spec), base+token; got != want {
			t.Errorf("spec %q: got %q, want %q", spec, got, want)
		}
	}
	unsigned := downloadURLObject(t, base, "&token=ticket")
	if got, want := BuildDownloadURLWithSpec(unsigned, "original"), base+"&token=ticket"; got != want {
		t.Errorf("unsigned original: got %q, want %q", got, want)
	}
}

func TestDownloadURLExplicitRenditionPreservesOtherBytes(t *testing.T) {
	const signed = "https://cdn.example/video?token=a%2fb%3D&sign=a+b%2F&basedata=c%26d"
	for _, tc := range []struct {
		name, base, spec, want string
	}{
		{"append", signed, "xWT111", signed + "&X-snsvideoflag=xWT111"},
		{"no query", "https://cdn.example/video", "xWT111", "https://cdn.example/video?X-snsvideoflag=xWT111"},
		{"replace duplicate flag", signed + "&X-snsvideoflag=old&X-snsvideoflag=other", "xWT113", signed + "&X-snsvideoflag=xWT113"},
		{"fragment", signed + "#fragment", "xWT111", signed + "&X-snsvideoflag=xWT111#fragment"},
		{"encoded flag name", signed + "&X%2Dsnsvideoflag=old", "xWT111", signed + "&X-snsvideoflag=xWT111"},
		{"escape spec", signed, "x&other=value", signed + "&X-snsvideoflag=x%26other%3Dvalue"},
		{"archive", "zip://weixin.qq.com?files=[]", "xWT111", "zip://weixin.qq.com?files=[]"},
		{"empty", "", "xWT111", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := downloadURLObject(t, tc.base, "")
			if got := BuildDownloadURLWithSpec(obj, tc.spec); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDownloadURLDoesNotInventVideoURLsForPicturesOrLiveStreams(t *testing.T) {
	for _, kind := range []string{"picture", "live"} {
		obj := downloadURLObject(t, "https://cdn.example/video?token=t", "")
		if kind == "picture" {
			obj.ObjectDesc.MediaType = wxchannels.MediaTypePicture
		} else {
			obj.LiveInfo = &wxchannels.ChannelsLiveInfo{}
		}
		if got := BuildDownloadURLWithSpec(obj, "xWT111"); got != "" {
			t.Errorf("%s: unexpected video URL %q", kind, got)
		}
	}
}
