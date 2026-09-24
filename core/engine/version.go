package engine

import (
	"strconv"
	"strings"
	"unicode"
)

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.ToLower(v), "v")
	return v
}

type versionPart struct {
	num  int
	text string
}

func parseVersionParts(v string) []versionPart {
	v = normalizeVersion(v)
	if v == "" {
		return nil
	}
	var parts []versionPart
	var buf strings.Builder
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		s := buf.String()
		buf.Reset()
		if n, err := strconv.Atoi(s); err == nil {
			parts = append(parts, versionPart{num: n})
		} else {
			parts = append(parts, versionPart{text: s})
		}
	}
	for _, r := range v {
		if unicode.IsDigit(r) {
			if buf.Len() > 0 {
				last := buf.String()
				if _, err := strconv.Atoi(last); err != nil {
					flush()
				}
			}
			buf.WriteRune(r)
		} else {
			flush()
			if !unicode.IsSpace(r) {
				buf.WriteRune(r)
			}
		}
	}
	flush()
	return parts
}

// versionCompare returns -1 if a<b, 0 if equal, 1 if a>b.
func versionCompare(a, b string) int {
	a = normalizeVersion(a)
	b = normalizeVersion(b)
	if a == b {
		return 0
	}
	pa := parseVersionParts(a)
	pb := parseVersionParts(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var left, right versionPart
		if i < len(pa) {
			left = pa[i]
		}
		if i < len(pb) {
			right = pb[i]
		}
		if left.text != "" || right.text != "" {
			ls, rs := left.text, right.text
			if ls == "" {
				ls = strconv.Itoa(left.num)
			}
			if rs == "" {
				rs = strconv.Itoa(right.num)
			}
			if ls != rs {
				return strings.Compare(ls, rs)
			}
			continue
		}
		if left.num != right.num {
			if left.num < right.num {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(a, b)
}

// UpdateAvailable reports whether latest is newer than installed.
func UpdateAvailable(installed, latest string) bool {
	installed = normalizeVersion(installed)
	latest = normalizeVersion(latest)
	if installed == "" || latest == "" || installed == latest {
		return false
	}
	return versionCompare(latest, installed) > 0
}
