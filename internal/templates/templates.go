// Package templates is luncur's one-click app gallery: embedded,
// code-reviewed YAML descriptions of an app (a pinned image) plus the
// addons, volumes and env it needs. This package only parses, validates
// and resolves placeholders; installing is the server's job.
package templates

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

//go:embed catalog/*.yaml
var catalogFS embed.FS

// Template is one gallery entry.
type Template struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Category    string            `json:"category"`
	App         App               `json:"app"`
	Volumes     []Volume          `json:"volumes,omitempty"`
	Addons      []Addon           `json:"addons,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// App is the template's container.
type App struct {
	Image      string `json:"image"`
	Port       int    `json:"port"`
	HealthPath string `json:"health_path,omitempty"`
	CPU        string `json:"cpu,omitempty"`
	Memory     string `json:"memory,omitempty"`
}

// Volume is a persistent volume the app mounts.
type Volume struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SizeGB int    `json:"size_gb"`
}

// Addon is a managed addon the template creates and attaches. Key is how
// env placeholders refer to it (${addon.<key>.host}); the addon itself is
// named <app>-<key>.
type Addon struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

var (
	nameRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	placeholderRe = regexp.MustCompile(`\$\{([^}]+)\}`)
	addonFields   = map[string]bool{"host": true, "port": true, "user": true, "password": true, "database": true, "url": true}
	addonTypes    = map[string]bool{"postgres": true, "redis": true, "minio": true}
)

// Validate checks a template's shape and every placeholder.
func (t Template) Validate() error {
	switch {
	case !nameRe.MatchString(t.Name):
		return fmt.Errorf("template name %q invalid", t.Name)
	case t.Title == "" || t.Description == "":
		return fmt.Errorf("%s: title and description required", t.Name)
	case t.App.Image == "" || !strings.Contains(t.App.Image, ":") || strings.HasSuffix(t.App.Image, ":latest"):
		return fmt.Errorf("%s: image must be pinned to a tag", t.Name)
	case t.App.Port < 1 || t.App.Port > 65535:
		return fmt.Errorf("%s: port out of range", t.Name)
	}
	keys := map[string]string{}
	for _, a := range t.Addons {
		if !nameRe.MatchString(a.Key) || !addonTypes[a.Type] {
			return fmt.Errorf("%s: addon %q (%s) invalid", t.Name, a.Key, a.Type)
		}
		keys[a.Key] = a.Type
	}
	for _, v := range t.Volumes {
		if !nameRe.MatchString(v.Name) || !strings.HasPrefix(v.Path, "/") || v.SizeGB < 1 {
			return fmt.Errorf("%s: volume %q invalid", t.Name, v.Name)
		}
	}
	for k, v := range t.Env {
		for _, m := range placeholderRe.FindAllStringSubmatch(v, -1) {
			if err := checkPlaceholder(m[1], keys); err != nil {
				return fmt.Errorf("%s: env %s: %w", t.Name, k, err)
			}
		}
	}
	return nil
}

func checkPlaceholder(p string, addons map[string]string) error {
	switch {
	case p == "app.url" || p == "app.host":
		return nil
	case strings.HasPrefix(p, "random:"):
		n, err := strconv.Atoi(strings.TrimPrefix(p, "random:"))
		if err != nil || n < 8 || n > 128 {
			return fmt.Errorf("${%s}: random length must be 8-128", p)
		}
		return nil
	case strings.HasPrefix(p, "addon."):
		parts := strings.Split(p, ".")
		if len(parts) != 3 || addons[parts[1]] == "" || !addonFields[parts[2]] {
			return fmt.Errorf("${%s}: unknown addon or field", p)
		}
		return nil
	}
	return fmt.Errorf("${%s}: unknown placeholder", p)
}

// All returns every embedded template, sorted by name. It panics on an
// invalid template: the catalog is part of the binary and covered by tests.
func All() []Template {
	entries, err := catalogFS.ReadDir("catalog")
	if err != nil {
		panic(err)
	}
	out := make([]Template, 0, len(entries))
	for _, e := range entries {
		b, err := catalogFS.ReadFile("catalog/" + e.Name())
		if err != nil {
			panic(err)
		}
		var t Template
		if err := yaml.UnmarshalStrict(b, &t); err != nil {
			panic(fmt.Sprintf("template %s: %v", e.Name(), err))
		}
		if err := t.Validate(); err != nil {
			panic(err)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the template named name.
func Find(name string) (Template, bool) {
	for _, t := range All() {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// Conn is one created addon's connection fields, for placeholders.
type Conn struct {
	Host, Port, User, Password, Database, URL string
}

// Resolve substitutes every placeholder in the template's env. extra
// (user-supplied env) wins over template keys.
func (t Template) Resolve(appURL, appHost string, addons map[string]Conn, extra map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(t.Env)+len(extra))
	var firstErr error
	for k, v := range t.Env {
		out[k] = placeholderRe.ReplaceAllStringFunc(v, func(m string) string {
			p := m[2 : len(m)-1]
			switch {
			case p == "app.url":
				return appURL
			case p == "app.host":
				return appHost
			case strings.HasPrefix(p, "random:"):
				n, _ := strconv.Atoi(strings.TrimPrefix(p, "random:"))
				buf := make([]byte, (n+1)/2)
				if _, err := rand.Read(buf); err != nil && firstErr == nil {
					firstErr = err
				}
				return hex.EncodeToString(buf)[:n]
			case strings.HasPrefix(p, "addon."):
				parts := strings.Split(p, ".")
				c, ok := addons[parts[1]]
				if !ok {
					if firstErr == nil {
						firstErr = fmt.Errorf("addon %q was not created", parts[1])
					}
					return ""
				}
				return map[string]string{"host": c.Host, "port": c.Port, "user": c.User, "password": c.Password, "database": c.Database, "url": c.URL}[parts[2]]
			}
			return m
		})
	}
	for k, v := range extra {
		out[k] = v
	}
	return out, firstErr
}
