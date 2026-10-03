package textx_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"koro/internal/platform/textx"
)

func TestTextx_TurkishLowerUpper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, lower, upper string
	}{
		{"ISPARTA", "ısparta", "ISPARTA"},
		{"İstanbul", "istanbul", "İSTANBUL"},
		{"ışık", "ışık", "IŞIK"},
		{"IŞIK", "ışık", "IŞIK"},
		{"çağla ÖZGÜR", "çağla özgür", "ÇAĞLA ÖZGÜR"},
		{"I\u0307zmir", "izmir", "İZMİR"}, // decomposed İ is composed first (NFC)
		{"Diyarbakır", "diyarbakır", "DİYARBAKIR"},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := textx.LowerTR(tt.in); got != tt.lower {
				t.Errorf("LowerTR(%q) = %q, want %q", tt.in, got, tt.lower)
			}
			if got := textx.UpperTR(tt.in); got != tt.upper {
				t.Errorf("UpperTR(%q) = %q, want %q", tt.in, got, tt.upper)
			}
		})
	}
	if strings.ToUpper("i") != "I" || textx.UpperTR("i") != "İ" {
		t.Fatal("stdlib upper-case differs from Turkish as expected (R-002 §1.6)")
	}
	eq := []struct {
		a, b string
		want bool
	}{
		{"ISPARTA", "ısparta", true},
		{"İstanbul", "ISTANBUL", false},
		{"İstanbul", "İSTANBUL", true},
		{"I", "i", false},
		{"Çağla", "ÇAĞLA", true},
	}
	for _, e := range eq {
		if got := textx.EqualFoldTR(e.a, e.b); got != e.want {
			t.Errorf("EqualFoldTR(%q, %q) = %v, want %v", e.a, e.b, got, e.want)
		}
	}
}

func TestTextx_EmailNormalize_NotTurkishLocale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"ISPARTA@okul.edu.tr", "isparta@okul.edu.tr"}, // Turkish rules would give "ısparta"
		{"  Ayse.Yilmaz@Example.TEST \n", "ayse.yilmaz@example.test"},
		{"İSMAİL@koro.test", "ismail@koro.test"}, // U+0130 → "i" (simple Unicode mapping)
		{"MIXED.Case+Tag@KORO.TEST", "mixed.case+tag@koro.test"},
		{"cafe\u0301@example.test", "café@example.test"}, // NFC
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got := textx.NormalizeEmail(tt.in)
			if got != tt.want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.ContainsRune(got, 'ı') {
				t.Fatalf("NormalizeEmail(%q) used Turkish casing: %q", tt.in, got)
			}
		})
	}
	if textx.LowerTR("ISPARTA@okul.edu.tr") == textx.NormalizeEmail("ISPARTA@okul.edu.tr") {
		t.Fatal("e-mail normalization must differ from Turkish lower-casing")
	}
}

func TestTextx_NormalizeText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
		wantErr        error
	}{
		{"trim and collapse", "  Ayşe \t  Nur\n Yılmaz  ", "Ayşe Nur Yılmaz", nil},
		{"NFC", "Gu\u0308l", "Gül", nil},
		{"nbsp is a space", "Ali\u00a0Veli", "Ali Veli", nil},
		{"zero-width removed", "Al\u200bi", "Ali", nil},
		{"NFC after removal", "e\u200b\u0301", "\u00e9", nil},
		{"emoji ZWJ kept", "Ekip 👩\u200d💻", "Ekip 👩\u200d💻", nil},
		{"only spaces", " \t\n ", "", nil},
		{"NUL rejected", "Ali\x00", "", textx.ErrInvalidText},
		{"escape rejected", "Ali\x1b[31m", "", textx.ErrInvalidText},
		{"bidi override rejected", "abc\u202egpj.exe", "", textx.ErrInvalidText},
		{"bidi isolate rejected", "a\u2067b", "", textx.ErrInvalidText},
		{"invalid UTF-8 rejected", "Ali\xff", "", textx.ErrInvalidText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := textx.NormalizeText(tt.in)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Fatalf("NormalizeText(%q) = %q, %v; want %q, %v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestTextx_Slug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"Çağla Işık Şenyürek", 0, "cagla-isik-senyurek"},
		{"İstanbul Teknik Üniversitesi", 0, "istanbul-teknik-universitesi"},
		{"  --Boğaziçi   Kulübü!! 2026 ", 0, "bogazici-kulubu-2026"},
		{"ISPARTA", 0, "isparta"},
		{"Ｆｕｌｌｗｉｄｔｈ ℃", 0, "fullwidth-c"},
		{"Hâkim Kâğıt", 0, "hakim-kagit"},
		{"a_b.c/d", 0, "a-b-c-d"},
		{"!!!", 0, ""},
		{"日本語", 0, ""},
		{"abc def ghi", 7, "abc-def"},
		{"abc def ghi", 8, "abc-def"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got := textx.Slug(tt.in, tt.maxLen)
			if got != tt.want {
				t.Fatalf("Slug(%q, %d) = %q, want %q", tt.in, tt.maxLen, got, tt.want)
			}
			if got != "" && !slugRe.MatchString(got) {
				t.Fatalf("Slug(%q) = %q does not match the slug CHECK", tt.in, got)
			}
		})
	}
}

func TestTextx_EscapeLikeAndSafeCell(t *testing.T) {
	t.Parallel()
	if got := textx.EscapeLike(`50%_off\x`); got != `50\%\_off\\x` {
		t.Fatalf("EscapeLike = %q", got)
	}
	cells := map[string]string{
		"=SUM(A1)": "'=SUM(A1)", "+90": "'+90", "-5": "'-5", "@cmd": "'@cmd", "\tx": "'\tx", "\rx": "'\rx",
		"Ali": "Ali", "": "", "5-3": "5-3",
	}
	for in, want := range cells {
		if got := textx.SafeCell(in); got != want {
			t.Errorf("SafeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzTextx_Slug(f *testing.F) {
	for _, s := range []string{"Çağla Işık", "--a--", "℃", "İ\u0307", "e\u200b\u0301", "\xff", ""} {
		f.Add(s, 80)
	}
	f.Fuzz(func(t *testing.T, s string, maxLen int) {
		got := textx.Slug(s, maxLen)
		if got != "" && !slugRe.MatchString(got) {
			t.Fatalf("Slug(%q) = %q does not match the slug CHECK", s, got)
		}
		if maxLen > 0 && len(got) > maxLen {
			t.Fatalf("Slug(%q, %d) = %q is too long", s, maxLen, got)
		}
		if n, err := textx.NormalizeText(s); err == nil {
			if !utf8.ValidString(n) || strings.Contains(n, "  ") || strings.TrimSpace(n) != n {
				t.Fatalf("NormalizeText(%q) = %q is not normalized", s, n)
			}
			if again, err := textx.NormalizeText(n); err != nil || again != n {
				t.Fatalf("NormalizeText is not idempotent: %q → %q → %q, %v", s, n, again, err)
			}
		}
	})
}
