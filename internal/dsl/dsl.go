// Package dsl defines the on-disk request format (.req.toml) and the
// workspace configuration files (collection.toml, folder.toml, environment
// files). Files are plain TOML so collections stay git-diffable.
package dsl

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Request is one .req.toml file.
type Request struct {
	Name       string            `toml:"name"`
	Method     string            `toml:"method"`
	URL        string            `toml:"url"`
	Query      map[string]string `toml:"query"`
	Headers    map[string]string `toml:"headers"`
	Vars       map[string]string `toml:"vars"`
	Auth       *Auth             `toml:"auth"`
	Body       *Body             `toml:"body"`
	Assertions []Assertion       `toml:"assertions"`

	// Path is where the request was loaded from (not serialized).
	Path string `toml:"-"`
}

// Auth holds the request auth helper config.
type Auth struct {
	Type     string `toml:"type"` // bearer | basic | apikey
	Token    string `toml:"token"`
	Username string `toml:"username"`
	Password string `toml:"password"`
	Key      string `toml:"key"`   // apikey header/query name
	Value    string `toml:"value"` // apikey value
	In       string `toml:"in"`    // header (default) | query
}

// Body holds the request body. Content and File are mutually exclusive.
type Body struct {
	Type    string `toml:"type"` // json | xml | raw | urlencoded | binary
	Content string `toml:"content"`
	File    string `toml:"file"`
}

// Assertion is one post-response check.
type Assertion struct {
	Type     string `toml:"type"` // status | jsonpath | header | contains | max_ms
	Path     string `toml:"path"`
	Name     string `toml:"name"`
	Equals   any    `toml:"equals"`
	Contains string `toml:"contains"`
	MaxMs    int64  `toml:"max_ms"`
}

// Config is a collection.toml or folder.toml: headers and vars that
// requests beneath it inherit.
type Config struct {
	Name    string            `toml:"name"`
	Headers map[string]string `toml:"headers"`
	Vars    map[string]string `toml:"vars"`
}

// Environment is an environments/<name>.toml file. Secret variables are
// listed by name only and resolved from the OS environment
// (RELAY_SECRET_<NAME>) so secret values never touch the repo.
type Environment struct {
	Vars    map[string]string `toml:"vars"`
	Secrets []string          `toml:"secrets"`
}

// LoadRequest parses one .req.toml file.
func LoadRequest(path string) (*Request, error) {
	var r Request
	if _, err := toml.DecodeFile(path, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Method == "" {
		r.Method = "GET"
	}
	r.Method = strings.ToUpper(r.Method)
	if r.URL == "" {
		return nil, fmt.Errorf("%s: missing url", path)
	}
	r.Path = path
	return &r, nil
}

// LoadConfig parses a collection.toml or folder.toml. A missing file is not
// an error; it returns nil.
func LoadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// LoadEnvironment parses an environment file.
func LoadEnvironment(path string) (*Environment, error) {
	var e Environment
	if _, err := toml.DecodeFile(path, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &e, nil
}
