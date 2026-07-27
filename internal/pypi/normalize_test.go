package pypi

import "testing"

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// PEP 503's own examples.
		{"friendly-bard", "friendly-bard"},
		{"Friendly-Bard", "friendly-bard"},
		{"FRIENDLY-BARD", "friendly-bard"},
		{"friendly.bard", "friendly-bard"},
		{"friendly_bard", "friendly-bard"},
		{"friendly--bard", "friendly-bard"},
		{"FrIeNdLy-._.-bArD", "friendly-bard"},
		// Dotted, underscored and consecutive-separator cases.
		{"zope.interface", "zope-interface"},
		{"zope-interface", "zope-interface"},
		{"Zope.Interface", "zope-interface"},
		{"Foo__Bar", "foo-bar"},
		{"a---b", "a-b"},
		{"a._-.b", "a-b"},
		{"", ""},
		{"numpy", "numpy"},
	}

	for _, tt := range tests {
		if got := NormalizeName(tt.in); got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
