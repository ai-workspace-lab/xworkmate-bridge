package acp

import "strings"

type imageVersionInfo struct {
	ImageRef string `json:"image_ref"`
	Tag      string `json:"tag,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Version  string `json:"version,omitempty"`
}

func ParseImageVersionInfo(imageRef string) imageVersionInfo {
	ref := strings.TrimSpace(imageRef)
	info := imageVersionInfo{ImageRef: ref}
	if ref == "" {
		return info
	}

	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	}

	tag := ref
	if idx := strings.LastIndex(tag, ":"); idx >= 0 && idx > strings.LastIndex(tag, "/") {
		tag = tag[idx+1:]
	}
	tag = strings.TrimSpace(tag)
	info.Tag = tag

	switch {
	case isHexCommit(tag):
		info.Commit = tag
		info.Version = tag
	default:
		info.Version = tag
	}

	return info
}

func isHexCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
