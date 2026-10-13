package breakcheck

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadManifest reads a breakcheck.yaml: a list of checks. Relative `file` and
// `dir` values are resolved against the manifest's own directory, so the
// manifest means the same thing whichever directory it is run from.
func LoadManifest(path string) ([]Check, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var checks []Check
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&checks); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(abs)
	seen := map[string]bool{}
	for i := range checks {
		c := &checks[i]
		if c.Name == "" {
			c.Name = fmt.Sprintf("#%d", i+1)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("%s: two checks are named %q", path, c.Name)
		}
		seen[c.Name] = true
		if err := validate(*c); err != nil {
			return nil, fmt.Errorf("%s: check %q: %w", path, c.Name, err)
		}
		if !filepath.IsAbs(c.File) {
			c.File = filepath.Join(base, c.File)
		}
		switch {
		case c.Dir == "":
			c.Dir = base
		case !filepath.IsAbs(c.Dir):
			c.Dir = filepath.Join(base, c.Dir)
		}
	}
	return checks, nil
}
