package config

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileMode is a configuration permission bitmask, always read as octal.
//
// Reading it as octal unconditionally is the whole point of the type. YAML
// would otherwise decide for itself: `0660` happens to resolve to octal
// because yaml.v3 parses ints with a base-0 rule, but `660` resolves to six
// hundred and sixty, which as a mode is 01224. Nobody writing file
// permissions means decimal, so both spellings mean 0660 here.
type FileMode fs.FileMode

// Std returns the wrapped standard file mode.
func (m FileMode) Std() fs.FileMode { return fs.FileMode(m) }

func (m FileMode) String() string { return "0" + strconv.FormatUint(uint64(m), 8) }

// UnmarshalYAML accepts 0660, 0o660, "0660" or 660, all as octal.
func (m *FileMode) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!str", "!!int":
		parsed, err := parseFileMode(node.Value)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		*m = parsed
		return nil

	case "!!null":
		// A key written with no value leaves the default in place.
		return nil

	default:
		return fmt.Errorf("line %d: expected an octal permission such as 0660, got %s",
			node.Line, node.Tag)
	}
}

// MarshalYAML writes the octal form, so a round-trip stays readable.
func (m FileMode) MarshalYAML() (any, error) { return m.String(), nil }

// parseFileMode reads an octal permission, tolerating the 0 and 0o prefixes.
func parseFileMode(s string) (FileMode, error) {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(s, "0o"), "0O")
	parsed, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an octal permission; write it like 0660", s)
	}
	if parsed > 0o777 {
		return 0, fmt.Errorf("%q sets more than permission bits; write it like 0660", s)
	}
	return FileMode(parsed), nil
}
