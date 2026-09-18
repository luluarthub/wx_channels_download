package wxchannelsadapter

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wx_channel/internal/database/model"
	"wx_channel/pkg/scraper/wxchannels"
)

// PickSpec returns the first advertised spec's FileFormat, or an empty string.
func PickSpec(obj *wxchannels.ChannelsObject) string {
	specs := obj.Spec
	if len(obj.ObjectDesc.Media) > 0 && len(obj.ObjectDesc.Media[0].Spec) > 0 {
		specs = obj.ObjectDesc.Media[0].Spec
	}
	if len(specs) > 0 {
		return specs[0].FileFormat
	}
	return ""
}

// BuildDownloadURLWithSpec returns the download URL for the given spec.
//
// Original URLs retain all CDN signature parameters and are never replaced by
// the highest advertised rendition. Explicit specs replace only X-snsvideoflag,
// preserving the byte encoding of the other query parameters.
// Picture archives and live streams are returned unchanged.
func BuildDownloadURLWithSpec(obj *wxchannels.ChannelsObject, spec string) string {
	base_url := ObjectURL(obj)

	if spec == "" || spec == "original" || strings.HasPrefix(base_url, "zip://") || obj.LiveInfo != nil {
		return base_url
	}
	if base_url == "" {
		return ""
	}
	base, fragment, has_fragment := strings.Cut(base_url, "#")
	path, raw_query, _ := strings.Cut(base, "?")
	parts := strings.Split(raw_query, "&")
	query := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decoded_key, err := url.QueryUnescape(key)
		if part != "" && (err != nil || decoded_key != "X-snsvideoflag") {
			query = append(query, part)
		}
	}
	query = append(query, "X-snsvideoflag="+url.QueryEscape(spec))
	result := path + "?" + strings.Join(query, "&")
	if has_fragment {
		result += "#" + fragment
	}
	return result
}

// DecryptKeyInt returns the video decrypt key as int, or 0 on failure.
func DecryptKeyInt(obj *wxchannels.ChannelsObject) int {
	if len(obj.ObjectDesc.Media) == 0 {
		return 0
	}
	key, err := strconv.Atoi(obj.ObjectDesc.Media[0].DecodeKey)
	if err != nil {
		return 0
	}
	return key
}

// ObjectTitle returns the object title with fallback logic (description → ID → timestamp).
func ObjectTitle(obj *wxchannels.ChannelsObject) string {
	if obj.LiveInfo != nil {
		return "直播"
	}
	title := strings.TrimSpace(obj.ObjectDesc.Description)
	if title != "" {
		return title
	}
	if strings.TrimSpace(obj.ID) != "" {
		return obj.ID
	}
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// DecryptKey exposes the legacy channels conversion capability through the
// registered handler, so callers do not need to import this package.
func (a *ChannelsAdapter) DecryptKey(content_json json.RawMessage) (int, error) {
	var obj wxchannels.ChannelsObject
	if err := json.Unmarshal(content_json, &obj); err != nil {
		return 0, err
	}
	return DecryptKeyInt(&obj), nil
}

// ConvertContent converts a raw channels object into the shared content model.
func (a *ChannelsAdapter) ConvertContent(content_json json.RawMessage) (*model.Content, error) {
	var obj wxchannels.ChannelsObject
	if err := json.Unmarshal(content_json, &obj); err != nil {
		return nil, err
	}
	content, _, err := ToContent(&obj)
	return content, err
}
