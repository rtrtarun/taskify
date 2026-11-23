package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/spf13/viper"
)

const defaultProfile = "default"

// profileKeys are the settings a profile holds. The credential is not one
// of them: it never goes into the config file (credentials.go).
var profileKeys = []string{"server", "tenant", "workspace"}

// Viper lowercases keys, so profile names are lower case, and a dot would
// split the key.
var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// configStore is config.yaml, read and written through Viper:
//
//	current_profile: default
//	profiles:
//	  default: {server: ..., tenant: ..., workspace: ...}
type configStore struct {
	path string
	v    *viper.Viper
}

// configDir is the directory holding config.yaml and the fallback
// credentials file: TASKIFY_CONFIG's directory, or the user config
// directory's taskctl folder.
func (a *app) configPath() (string, error) {
	if p := a.getenv("TASKIFY_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no user config directory: %w", err)
	}
	return filepath.Join(dir, "taskctl", "config.yaml"), nil
}

func (a *app) loadConfig() (*configStore, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	v.SetConfigPermissions(0o600)
	if err := v.ReadInConfig(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &configStore{path: path, v: v}, nil
}

func (c *configStore) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	return c.v.WriteConfigAs(c.path)
}

func (c *configStore) current() string {
	if p := c.v.GetString("current_profile"); p != "" {
		return p
	}
	return defaultProfile
}

func (c *configStore) get(profile, key string) string {
	return c.v.GetString("profiles." + profile + "." + key)
}

func (c *configStore) set(profile, key, value string) {
	c.v.Set("profiles."+profile+"."+key, value)
}

func (c *configStore) profiles() []string {
	var names []string
	for n := range c.v.GetStringMap("profiles") {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// settings are the resolved values for one run, each with where it came from.
type settings struct {
	profile, server, workspace, tenant string
	sources                            map[string]string
}

// resolve applies the precedence of section 9.5: a flag, then an
// environment variable, then the profile. The token has no flag (it would
// show in the process list); see token().
func (a *app) resolve(cfg *configStore) (settings, error) {
	s := settings{sources: map[string]string{}}
	pick := func(name, flag, env, fromProfile string) string {
		switch {
		case flag != "":
			s.sources[name] = "flag"
			return flag
		case env != "":
			s.sources[name] = "environment"
			return env
		case fromProfile != "":
			s.sources[name] = "profile"
			return fromProfile
		}
		s.sources[name] = "unset"
		return ""
	}
	s.profile = pick("profile", a.profileFlag, a.getenv("TASKIFY_PROFILE"), cfg.current())
	if !profileName.MatchString(s.profile) {
		return s, fail(exitUsage, "invalid profile name %q: use lower-case letters, digits, '-' and '_'", s.profile)
	}
	s.server = pick("server", a.serverFlag, a.getenv("TASKIFY_SERVER"), cfg.get(s.profile, "server"))
	s.workspace = pick("workspace", a.workspaceFlag, "", cfg.get(s.profile, "workspace"))
	s.tenant = pick("tenant", "", "", cfg.get(s.profile, "tenant"))
	return s, nil
}
