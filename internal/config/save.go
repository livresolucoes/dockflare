package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const savedHeader = `# DockFlare configuration.
#
# This file is rewritten by the web UI, which does not preserve comments.
# Secrets are never stored here: TUNNEL_TOKEN, CLOUDFLARE_API_TOKEN and
# DOCKFLARE_UI_TOKEN come from the environment. See the README.

`

// fileDoc is the on-disk shape. It exists separately from Config so that Save
// cannot write a field Config holds only in memory — above all the tokens.
type fileDoc struct {
	Token      string   `yaml:"token,omitempty"`
	Containers []string `yaml:"containers"`
	Routes     []Route  `yaml:"routes,omitempty"`
	ManageDNS  bool     `yaml:"manage_dns"`
	WebUI      *WebUI   `yaml:"web_ui,omitempty"`
}

// Save writes cfg to path atomically.
//
// Secrets never reach the file. The `token:` field is carried over from whatever
// is already on disk — deliberately not from cfg.Token, which may hold the value
// of TUNNEL_TOKEN and would leak the environment secret into a file the user
// might commit. API and UI tokens have no YAML representation at all.
func Save(path string, cfg *Config) error {
	token, err := tokenOnDisk(path)
	if err != nil {
		return err
	}

	doc := fileDoc{
		Token:      token,
		Containers: cfg.Containers,
		Routes:     routesForFile(cfg.Routes),
		ManageDNS:  cfg.ManageDNS,
	}
	// Omit the whole section when the UI is off, so a default install keeps a
	// file that looks like it did before the feature existed.
	if cfg.WebUI.Enabled {
		ui := cfg.WebUI
		doc.WebUI = &ui
	}
	if doc.Containers == nil {
		doc.Containers = []string{}
	}

	encoded, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	return writeAtomic(path, append([]byte(savedHeader), encoded...))
}

// routesForFile drops values that only exist because Load filled them in, so a
// round-trip through the UI does not litter the file with `origin_scheme: http`
// on every route.
func routesForFile(routes []Route) []Route {
	out := make([]Route, len(routes))
	copy(out, routes)
	for i := range out {
		if out[i].OriginScheme == SchemeHTTP {
			out[i].OriginScheme = ""
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tokenOnDisk reads just the `token:` field of the current file. A missing file
// is not an error: the UI is allowed to create the config from scratch.
func tokenOnDisk(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading config: %w", err)
	}
	var existing struct {
		Token string `yaml:"token"`
	}
	if err := yaml.Unmarshal(data, &existing); err != nil {
		return "", fmt.Errorf("parsing config: %w", err)
	}
	return existing.Token, nil
}

// writeAtomic replaces path via a temp file and a rename, so a crash or a
// concurrent read never sees a half-written config.
//
// The rename is what requires the config *directory* to be bind-mounted rather
// than the file: renaming onto a single-file bind mount cannot work.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.yml")
	if err != nil {
		return fmt.Errorf("writing config: %w — is %s mounted read-write? "+
			"the web UI needs the directory mounted, not the file", err, dir)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing config: %w — if %s is a single-file bind mount, "+
			"mount the directory instead", err, path)
	}
	return nil
}

// CheckWritable reports whether Save could succeed, so the UI can warn at
// startup instead of failing on the user's first click.
func CheckWritable(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".writecheck-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)

	// The directory being writable is not enough: the file itself can be a
	// read-only bind mount, which only the rename would discover.
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("%s is read-only", path)
	}
	return nil
}
