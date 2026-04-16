package nodeexporter

import (
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "alpine",
			input: "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.18.0\n",
			want:  map[string]string{"NAME": "Alpine Linux", "ID": "alpine", "VERSION_ID": "3.18.0"},
		},
		{
			name:  "debian with quoted values",
			input: "NAME=\"Debian GNU/Linux\"\nID=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n",
			want:  map[string]string{"NAME": "Debian GNU/Linux", "ID": "debian", "VERSION_ID": "12", "VERSION_CODENAME": "bookworm", "PRETTY_NAME": "Debian GNU/Linux 12 (bookworm)"},
		},
		{
			name:  "ubuntu with id_like and variant",
			input: "ID=ubuntu\nID_LIKE=debian\nVARIANT_ID=server\nVARIANT=\"Server\"\nBUILD_ID=20240101\nIMAGE_ID=ubuntu-server\nIMAGE_VERSION=24.04\n",
			want:  map[string]string{"ID": "ubuntu", "ID_LIKE": "debian", "VARIANT_ID": "server", "VARIANT": "Server", "BUILD_ID": "20240101", "IMAGE_ID": "ubuntu-server", "IMAGE_VERSION": "24.04"},
		},
		{
			name:  "wolfi",
			input: "ID=wolfi\nNAME=\"Wolfi\"\nPRETTY_NAME=\"Wolfi\"\n",
			want:  map[string]string{"ID": "wolfi", "NAME": "Wolfi", "PRETTY_NAME": "Wolfi"},
		},
		{
			name:  "comments and blank lines ignored",
			input: "# This is a comment\n\nID=ubuntu\n",
			want:  map[string]string{"ID": "ubuntu"},
		},
		{
			name:  "missing ID field",
			input: "NAME=SomeOS\nVERSION=1.0\n",
			want:  map[string]string{"NAME": "SomeOS", "VERSION": "1.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseOSRelease(strings.NewReader(tt.input))
			for k, wantV := range tt.want {
				if got := result[k]; got != wantV {
					t.Errorf("ParseOSRelease()[%q] = %q, want %q", k, got, wantV)
				}
			}
		})
	}
}
