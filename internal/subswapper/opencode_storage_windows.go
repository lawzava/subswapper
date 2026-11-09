package subswapper

import (
	"errors"
	"os"
)

func openOpenCodePlaceholder(string) (*os.File, error) {
	return nil, errors.New("OpenCode placeholder permission checks require POSIX")
}
