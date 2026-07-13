package subswapper

import (
	"errors"
	"os"
	"path/filepath"
)

const sampleConfig = `{
  "monitor": {
    "interval": "5m",
    "auto_switch": true
  },
  "services": [
    {
      "name": "claude",
      "kind": "claude",
      "display_name": "Claude Code"
    },
    {
      "name": "codex",
      "kind": "codex",
      "display_name": "Codex"
    }
  ]
}
`

func WriteSampleConfig(path string) error {
	targetPath := ExpandPath(path)
	if _, err := os.Stat(targetPath); err == nil {
		return errors.New("config already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(targetPath, []byte(sampleConfig), 0o600)
}
