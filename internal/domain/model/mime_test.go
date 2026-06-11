package model

import "testing"

func TestNormalizeMIME(t *testing.T) {
	cases := []struct{ in, want string }{
		{"text/plain", "text/plain"},
		{"Text/Plain; charset=utf-8", "text/plain"},
		{"  APPLICATION/ZIP ", "application/zip"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeMIME(c.in); got != c.want {
			t.Errorf("NormalizeMIME(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsGenericMIME(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"application/zip", true},
		{"application/octet-stream", true},
		{"text/plain", true},
		{"Text/Plain; charset=utf-8", true}, // normalized before lookup
		{"APPLICATION/ZIP", true},
		{"application/vnd.openxmlformats-officedocument.presentationml.presentation", false},
		{"image/png", false},
		{"text/markdown", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsGenericMIME(c.in); got != c.want {
			t.Errorf("IsGenericMIME(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOfficeMIMEForName(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		wantOK bool
	}{
		{"Deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", true},
		{"report.DOCX", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
		{"sheet.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", true},
		{"notes.odt", "application/vnd.oasis.opendocument.text", true},
		{"data.ods", "application/vnd.oasis.opendocument.spreadsheet", true},
		{"slides.odp", "application/vnd.oasis.opendocument.presentation", true},
		{"archive.zip", "", false},
		{"noextension", "", false},
		{"", "", false},
		{"trick.pptx.txt", "", false}, // only the final extension counts
	}
	for _, c := range cases {
		got, ok := OfficeMIMEForName(c.name)
		if got != c.want || ok != c.wantOK {
			t.Errorf("OfficeMIMEForName(%q) = (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
