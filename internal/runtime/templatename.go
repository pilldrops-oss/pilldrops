package runtime

import (
	"errors"
	"fmt"
)

// ErrInvalidTemplateName means the id is not legal at all, unlike
// ErrTemplateUnknown. Kept separate so the response never lists bootable
// templates for a string that was written to probe the filesystem.
var ErrInvalidTemplateName = errors.New("invalid template name")

// MaxTemplateNameLen bounds a template id.
const MaxTemplateNameLen = 64

// ValidateTemplateName rejects any id that is not `[a-z0-9][a-z0-9._-]*`.
// Template ids are joined into host paths and filepath.Join cleans ".." rather
// than failing; the alphanumeric first character makes ".." unrepresentable.
func ValidateTemplateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidTemplateName)
	}
	if len(name) > MaxTemplateNameLen {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidTemplateName, MaxTemplateNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case (c == '.' || c == '-' || c == '_') && i > 0:
			continue
		}
		return fmt.Errorf("%w: %q at position %d; a template id must match [a-z0-9][a-z0-9._-]*",
			ErrInvalidTemplateName, string(rune(c)), i)
	}
	return nil
}
