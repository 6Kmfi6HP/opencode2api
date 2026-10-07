package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// i18nDictStart / i18nDictEnd bracket the JSON dictionary inside web/i18n.js
// (embedded as adminI18nJS):
//
//	window.I18N_DICT = /*I18N_DICT_START*/{"zh":{...},"en":{...}}/*I18N_DICT_END*/;
const (
	i18nDictStart = "/*I18N_DICT_START*/"
	i18nDictEnd   = "/*I18N_DICT_END*/"
)

// i18nDict extracts and decodes the embedded i18n dictionary as
// language -> key -> translation.
func i18nDict(t *testing.T) map[string]map[string]string {
	t.Helper()
	start := strings.Index(adminI18nJS, i18nDictStart)
	if start < 0 {
		t.Fatalf("adminI18nJS does not contain %s", i18nDictStart)
	}
	start += len(i18nDictStart)
	end := strings.Index(adminI18nJS[start:], i18nDictEnd)
	if end < 0 {
		t.Fatalf("adminI18nJS does not contain %s", i18nDictEnd)
	}
	var dict map[string]map[string]string
	if err := json.Unmarshal([]byte(adminI18nJS[start:start+end]), &dict); err != nil {
		t.Fatalf("adminI18nJS dictionary is not valid JSON: %v", err)
	}
	return dict
}

// i18nLanguages lists the languages defined in the dictionary, sorted.
func i18nLanguages(dict map[string]map[string]string) []string {
	langs := make([]string, 0, len(dict))
	for lang := range dict {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

// TestI18N_KeyParity asserts the zh and en dictionaries cover exactly the
// same key set, so no language silently misses a translation.
func TestI18N_KeyParity(t *testing.T) {
	dict := i18nDict(t)
	zh, en := dict["zh"], dict["en"]
	if zh == nil {
		t.Fatalf("i18n dictionary has no \"zh\" language; got %v", i18nLanguages(dict))
	}
	if en == nil {
		t.Fatalf("i18n dictionary has no \"en\" language; got %v", i18nLanguages(dict))
	}
	var missingInEn, missingInZh []string
	for key := range zh {
		if _, ok := en[key]; !ok {
			missingInEn = append(missingInEn, key)
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			missingInZh = append(missingInZh, key)
		}
	}
	if len(missingInEn) == 0 && len(missingInZh) == 0 {
		return
	}
	sort.Strings(missingInEn)
	sort.Strings(missingInZh)
	t.Fatalf("i18n dictionary key sets differ:\n\tkeys missing in en (%d): %s\n\tkeys missing in zh (%d): %s",
		len(missingInEn), strings.Join(missingInEn, ", "),
		len(missingInZh), strings.Join(missingInZh, ", "))
}

// TestI18N_NoChineseInHTML asserts the embedded HTML files carry no Chinese
// characters (U+4E00 to U+9FFF): all UI Chinese must live in the i18n
// dictionary so the pages start language-neutral and are filled in by
// i18n.js at load time.
func TestI18N_NoChineseInHTML(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{"web/admin.html", adminHTML},
		{"web/login.html", adminLoginHTML},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			var samples []string
			for i, r := range tc.html {
				if r < '一' || r > '鿿' {
					continue
				}
				count++
				if len(samples) >= 10 {
					continue
				}
				lo, hi := i, i+len(string(r))
				for pad := 0; pad < 20 && (lo > 0 || hi < len(tc.html)); pad++ {
					if lo > 0 {
						lo--
					}
					if hi < len(tc.html) {
						hi++
					}
				}
				samples = append(samples, fmt.Sprintf("U+%04X %q", r, tc.html[lo:hi]))
			}
			if count > 0 {
				t.Errorf("%s contains %d Chinese rune(s) in U+4E00-U+9FFF; all UI Chinese must live in the i18n dictionary (web/i18n.js), not in the static markup. Samples: %s",
					tc.name, count, strings.Join(samples, "; "))
			}
		})
	}
}

// i18n reference patterns: data-i18n attributes in the static markup and
// quoted string arguments of t(...) calls in inline scripts. Go's regexp
// (RE2) has no backreferences, so each quote style of the t() argument gets
// its own pattern; \bt\( avoids matching identifiers such as split( or
// parseInt(.
var (
	i18nAttrRefRe   = regexp.MustCompile(`data-i18n(?:-placeholder|-title)?="([^"]+)"`)
	i18nCallRefReDQ = regexp.MustCompile(`\bt\("([^"]+)"`)
	i18nCallRefReSQ = regexp.MustCompile(`\bt\('([^']+)'`)
)

// TestI18N_ReferencesResolve asserts every i18n key referenced by the static
// HTML (data-i18n / data-i18n-placeholder / data-i18n-title attributes and
// t("...") calls) exists in both the zh and en dictionaries.
func TestI18N_ReferencesResolve(t *testing.T) {
	dict := i18nDict(t)
	zh, en := dict["zh"], dict["en"]
	if zh == nil || en == nil {
		t.Fatalf("i18n dictionary must define both zh and en; got %v", i18nLanguages(dict))
	}
	tests := []struct {
		name string
		html string
	}{
		{"web/admin.html", adminHTML},
		{"web/login.html", adminLoginHTML},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			referenced := map[string]struct{}{}
			for _, re := range []*regexp.Regexp{i18nAttrRefRe, i18nCallRefReDQ, i18nCallRefReSQ} {
				for _, match := range re.FindAllStringSubmatch(tc.html, -1) {
					referenced[match[1]] = struct{}{}
				}
			}
			var missing []string
			for key := range referenced {
				if _, ok := zh[key]; !ok {
					missing = append(missing, key+" (zh)")
				}
				if _, ok := en[key]; !ok {
					missing = append(missing, key+" (en)")
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s references %d i18n key(s); missing from dictionary: %s",
					tc.name, len(referenced), strings.Join(missing, ", "))
			}
		})
	}
}
