package nodeexporter

import (
	"bufio"
	"io"
	"strings"
)

// ParseOSRelease parses the key=value format of an /etc/os-release file.
// Values may be optionally quoted with single or double quotes (systemd spec).
// Comment lines and blank lines are ignored.
func ParseOSRelease(r io.Reader) (map[string]string, error) {
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
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[0] == val[len(val)-1] {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
