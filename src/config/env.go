package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// toSnakeCase converts CamelCase or mixedCase names to snake_case
func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// FromEnv constructs a Configuration from environment variables using the
// ELYTRA_ prefix. It will apply default values then override any struct
// fields where corresponding environment variables exist. After loading it
// sets the global configuration instance.
func FromEnv() error {
	c, err := NewAtPath("")
	if err != nil {
		return err
	}

	if err := populateFromEnv(reflect.ValueOf(c).Elem(), []string{}, "ELYTRA"); err != nil {
		return err
	}

	// Token fields have dedicated env names.
	c.Token = Token{
		ID:    os.Getenv("ELYTRA_TOKEN_ID"),
		Token: os.Getenv("ELYTRA_TOKEN"),
	}
	if c.Token.ID == "" {
		c.Token.ID = c.AuthenticationTokenId
	}
	if c.Token.Token == "" {
		c.Token.Token = c.AuthenticationToken
	}

	// Expand any token values that may reference files or other envs.
	if c.Token.ID != "" {
		if v, err := Expand(c.Token.ID); err == nil {
			c.Token.ID = v
		}
	}
	if c.Token.Token != "" {
		if v, err := Expand(c.Token.Token); err == nil {
			c.Token.Token = v
		}
	}

	Set(c)
	return nil
}

// MergeEnv fills zero-valued fields in the provided Configuration by reading
// environment variables. It does not overwrite non-zero values. The same
// env naming rules as FromEnv are used (prefix + hierarchical yaml/field names
// joined with underscores).
func MergeEnv(c *Configuration) error {
	if c == nil {
		return fmt.Errorf("config: nil configuration passed to MergeEnv")
	}
	return populateFromEnv(reflect.ValueOf(c).Elem(), []string{}, "ELYTRA")
}

// populateFromEnv recursively iterates a struct value and sets any exported
// field for which an environment variable exists. The env key is formed by
// joining prefix with the accumulated path of yaml tags (or field names).
func populateFromEnv(v reflect.Value, parts []string, prefix string) error {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if !fv.CanSet() {
			// ignore unexported
			continue
		}
		ft := t.Field(i)

		// prefer yaml tag, then json tag, then field name
		name := ft.Tag.Get("yaml")
		if name == "" {
			name = ft.Tag.Get("json")
		}
		if name == "" {
			name = ft.Name
		}
		// Some tags include options like `yaml:"host"` or `yaml:"-"`.
		name = strings.Split(name, ",")[0]
		if name == "-" || name == "" {
			name = ft.Name
		}

		newParts := append(parts, name)
		// Primary key uses yaml/json tag path
		envKeyTag := prefix + "_" + strings.ToUpper(strings.ReplaceAll(strings.Join(newParts, "_"), "-", "_"))
		// Secondary key uses the Go field names joined and converted to SNAKE_CASE
		// Convert CamelCase field names to SNAKE_CASE so PanelLocation -> PANEL_LOCATION
		fieldName := toSnakeCase(ft.Name)
		fieldParts := append(parts, fieldName)
		envKeyField := prefix + "_" + strings.ToUpper(strings.ReplaceAll(strings.Join(fieldParts, "_"), "-", "_"))

		// Check both keys; envValue will be the first non-empty one found.
		var envValue string
		if v := os.Getenv(envKeyTag); v != "" {
			envValue = v
		} else if v := os.Getenv(envKeyField); v != "" {
			envValue = v
		}

		switch fv.Kind() {
		case reflect.Struct:
			// handle time.Time etc by attempting to read raw env first
			// If env exists for this struct, try json unmarshal into it.
			if envValue != "" {
				if err := json.Unmarshal([]byte(envValue), fv.Addr().Interface()); err != nil {
					// fallback: skip if we cannot parse
					continue
				}
				continue
			}
			if err := populateFromEnv(fv, newParts, prefix); err != nil {
				return err
			}
		case reflect.String:
			if envValue != "" {
				fv.SetString(envValue)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if envValue != "" {
				if iv, err := strconv.ParseInt(envValue, 10, 64); err == nil {
					fv.SetInt(iv)
				}
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if envValue != "" {
				if uv, err := strconv.ParseUint(envValue, 10, 64); err == nil {
					fv.SetUint(uv)
				}
			}
		case reflect.Bool:
			if envValue != "" {
				if bv, err := strconv.ParseBool(envValue); err == nil {
					fv.SetBool(bv)
				}
			}
		case reflect.Slice:
			if envValue != "" {
				// for []string expect comma separated values
				if fv.Type().Elem().Kind() == reflect.String {
					parts := []string{}
					for _, p := range strings.Split(envValue, ",") {
						p = strings.TrimSpace(p)
						if p != "" {
							parts = append(parts, p)
						}
					}
					fv.Set(reflect.ValueOf(parts))
				} else {
					// attempt json unmarshal for more complex slices
					if err := json.Unmarshal([]byte(envValue), fv.Addr().Interface()); err == nil {
						// ok
					}
				}
			}
		case reflect.Map, reflect.Ptr, reflect.Interface:
			if envValue != "" {
				// Try to decode JSON for maps and complex types.
				if err := json.Unmarshal([]byte(envValue), fv.Addr().Interface()); err == nil {
					// success
				}
			}
		default:
			// unsupported kinds are ignored
		}
	}
	return nil
}

// ValidateRequired ensures that essential configuration values are present.
// It returns an error describing missing values if validation fails.
func ValidateRequired(c *Configuration) error {
	missing := []string{}
	if c.PanelLocation == "" {
		missing = append(missing, "ELYTRA_PANEL_LOCATION")
	}
	if c.Token.ID == "" {
		missing = append(missing, "ELYTRA_TOKEN_ID")
	}
	if c.Token.Token == "" {
		missing = append(missing, "ELYTRA_TOKEN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}
