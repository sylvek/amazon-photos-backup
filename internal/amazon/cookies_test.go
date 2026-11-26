package amazon

import (
	"reflect"
	"testing"
)

func TestParseCookiesVariants(t *testing.T) {
	want := map[string]string{"session-id": "123-456", "ubid-acbfr": "u=1", "at-acbfr": "Atza|abc"}
	inputs := map[string]string{
		"header":      "session-id=123-456; ubid-acbfr=u=1; at-acbfr=Atza|abc",
		"prefixed":    "Cookie: session-id=123-456; ubid-acbfr=u=1; at-acbfr=Atza|abc\n",
		"lowerprefix": "cookie:session-id=123-456;ubid-acbfr=u=1;at-acbfr=Atza|abc",
		"quoted":      "'session-id=123-456; ubid-acbfr=u=1; at-acbfr=Atza|abc'\r\n",
		"jsonobject":  `{"session-id":"123-456","ubid-acbfr":"u=1","at-acbfr":"Atza|abc"}`,
		"jsonarray":   `[{"name":"session-id","value":"123-456","domain":".amazon.fr"},{"name":"ubid-acbfr","value":"u=1"},{"name":"at-acbfr","value":"Atza|abc"}]`,
	}
	for name, in := range inputs {
		got, err := ParseCookies(in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	for _, bad := range []string{"", "   \n", "not a cookie", "{}", "[]", `{"a":`} {
		if got, err := ParseCookies(bad); err == nil {
			t.Errorf("ParseCookies(%q) = %v, want error", bad, got)
		}
	}
}

func TestDetectTLD(t *testing.T) {
	cases := []struct {
		key, tld string
		ok       bool
	}{
		{"at-acbfr", "fr", true},
		{"at-acbde", "de", true},
		{"at-acbuk", "co.uk", true},
		{"at-acbjp", "co.jp", true},
		{"at_main", "com", true},
		{"session-id", "", false},
	}
	for _, c := range cases {
		tld, ok := DetectTLD(map[string]string{c.key: "x", "other": "y"})
		if tld != c.tld || ok != c.ok {
			t.Errorf("DetectTLD(%s) = %q,%v want %q,%v", c.key, tld, ok, c.tld, c.ok)
		}
	}
}

func TestMissingCookies(t *testing.T) {
	full := map[string]string{"session-id": "1", "ubid-acbfr": "2", "at-acbfr": "3"}
	if m := MissingCookies(full); len(m) != 0 {
		t.Errorf("full set: missing %v", m)
	}
	us := map[string]string{"session-id": "1", "ubid_main": "2", "at_main": "3"}
	if m := MissingCookies(us); len(m) != 0 {
		t.Errorf("US set: missing %v", m)
	}
	// at-acbfr is HttpOnly: the classic mistake is copying document.cookie, which lacks it.
	noAT := map[string]string{"session-id": "1", "ubid-acbfr": "2"}
	if m := MissingCookies(noAT); !reflect.DeepEqual(m, []string{"at-acbfr"}) {
		t.Errorf("missing at: got %v", m)
	}
	if m := MissingCookies(map[string]string{"foo": "bar"}); len(m) != 3 {
		t.Errorf("unrelated cookies: got %v, want 3 missing", m)
	}
}
