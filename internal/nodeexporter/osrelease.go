package nodeexporter

import (
	"bufio"
	"io"
	"strings"
)

// ParseOSRelease parses the key=value format of an /etc/os-release file.
// Values may be optionally quoted with double quotes. Comment lines and blank
// lines are ignored.
func ParseOSRelease(r io.Reader) map[string]string {
	result := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		result[key] = strings.Trim(val, `"`)
	}
	return result
}
