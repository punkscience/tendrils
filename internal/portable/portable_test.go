package portable

import "testing"

func TestName(t *testing.T) {
	cases := []struct {
		desc string
		in   string
		want string
	}{
		{"already portable is untouched", "Various Artists - Covert-Two", "Various Artists - Covert-Two"},
		{"colon becomes a hyphen", "Various Artists - Covert:Two", "Various Artists - Covert-Two"},
		{"angle brackets become parentheses", "Track <live>", "Track (live)"},
		{"double quote becomes an apostrophe", `Say "hello"`, "Say 'hello'"},
		{"pipe becomes a hyphen", "A|B", "A-B"},
		{"question mark becomes an underscore", "What?.flac", "What_.flac"},
		{"asterisk becomes an underscore", "mix*.mp3", "mix_.mp3"},
		{"backslash becomes a hyphen", `AC\DC`, "AC-DC"},
		{"every illegal character at once", `a<b>c:d"e|f?g*h`, "a(b)c-d'e-f_g_h"},
		{"control characters are dropped", "tab\there", "tabhere"},
		{"trailing dot is trimmed", "album.", "album"},
		{"trailing space is trimmed", "album ", "album"},
		{"trailing dots and spaces together", "album. . ", "album"},
		{"interior dots and spaces survive", "a. b.flac", "a. b.flac"},
		{"leading dot survives", ".tendrilsignore", ".tendrilsignore"},
		{"reserved name gets a suffix", "NUL", "NUL_"},
		{"reserved name is case-insensitive", "nul", "nul_"},
		{"reserved name with extension", "COM1.txt", "COM1_.txt"},
		{"reserved-looking but not reserved", "CONSOLE", "CONSOLE"},
		{"nothing left becomes a placeholder", "...", "_"},
		{"only illegal characters", "?*", "__"},
		{"empty stays empty", "", ""},
		{"dot is path syntax, not a name", ".", "."},
		{"dotdot is path syntax, not a name", "..", ".."},
		{"non-ASCII is left alone", "Kanshō- Creation", "Kanshō- Creation"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := Name(c.in); got != c.want {
				t.Errorf("Name(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A rewritten name must itself be portable, or a second pass would rename it
// again and the tree would never settle.
func TestNameIsIdempotent(t *testing.T) {
	inputs := []string{
		"Covert:Two", `a<b>c:d"e|f?g*h`, "album. . ", "NUL", "...", "?*", "\x01\x02",
		"COM9.tar.gz", `AC\DC`, "trailing ..", "ok.flac",
	}
	for _, in := range inputs {
		once := Name(in)
		if twice := Name(once); twice != once {
			t.Errorf("Name(%q) = %q, but Name(%q) = %q — not idempotent", in, once, once, twice)
		}
		if !IsName(once) {
			t.Errorf("Name(%q) = %q, which IsName reports as unportable", in, once)
		}
	}
}

func TestPath(t *testing.T) {
	cases := []struct {
		desc string
		in   string
		want string
	}{
		{"portable path is untouched",
			"music/dj/album/track.flac", "music/dj/album/track.flac"},
		{"bad directory component",
			"music/Covert:Two/track.flac", "music/Covert-Two/track.flac"},
		{"bad file component",
			"music/album/what?.flac", "music/album/what_.flac"},
		{"bad at several depths",
			"a:b/c?d/e|f.flac", "a-b/c_d/e-f.flac"},
		{"root-level file", "what?.flac", "what_.flac"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := Path(c.in); got != c.want {
				t.Errorf("Path(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Separators carry the path's shape: a rewrite that changed the number of
// components would move the file to a different directory.
func TestPathPreservesDepth(t *testing.T) {
	const in = "a:b/c?d/e|f.flac"
	got := Path(in)
	if want := 2; countSlash(got) != want {
		t.Fatalf("Path(%q) = %q with %d separators, want %d", in, got, countSlash(got), want)
	}
}

func TestIsPath(t *testing.T) {
	if !IsPath("music/album/track.flac") {
		t.Error("IsPath said a clean path is unportable")
	}
	if IsPath("music/Covert:Two/track.flac") {
		t.Error("IsPath said a path with a colon is portable")
	}
}

func countSlash(s string) int {
	n := 0
	for _, r := range s {
		if r == '/' {
			n++
		}
	}
	return n
}
