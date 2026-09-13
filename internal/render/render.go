// Package render evaluates a manifest as a text/template and fills its placeholders with values
// from Terraform state. The mechanism is the same as ecspresso's tfstate integration.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"unicode/utf8"

	"github.com/fujiwara/tfstate-lookup/tfstate"
)

// DefaultStateName is the state name used for {{ tfstate "addr" }} (the name omitted).
const DefaultStateName = "default"

// validName matches the strings accepted as the name of a named state. The name becomes, as is,
// the prefix of the template function name {{ <name>tfstate }}, so it is limited to strings that
// are valid Go identifiers (an ASCII letter or _ first, then ASCII letters, digits or _).
// Registering an invalid name as it is makes text/template panic.
var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsValidName reports whether name is valid as the name of a named state (= the prefix of the
// template function name). The flag path and the config path share this constraint.
func IsValidName(name string) bool {
	return validName.MatchString(name)
}

// Source is the location of a named Terraform state. Location is a local path or a URL such as
// gs:// or s3://.
type Source struct {
	Name     string
	Location string
}

// Render evaluates a manifest as a text/template. Only the states that a tfstate function in the
// template actually uses are read (on their first reference).
//
// Functions are registered per state (the same approach as ecspresso's func_prefix).
// The default state gets {{ tfstate "addr" }} / {{ tfstatef "fmt" args }}; a named state gets
// {{ <name>tfstate "addr" }} / {{ <name>tfstatef ... }}, with the name used verbatim as the prefix.
func Render(ctx context.Context, manifest []byte, sources []Source) ([]byte, error) {
	funcs := template.FuncMap{
		"env":         envFunc,
		"must_env":    mustEnvFunc,
		"json_escape": jsonEscapeFunc,
	}

	for _, s := range sources {
		// The default state has no prefix; any other state uses its name verbatim as the prefix.
		// The name becomes a template function name (<name>tfstate), so a name that is not a
		// valid Go identifier makes template.Funcs panic. Reject it here with a clear error.
		prefix := ""
		if s.Name != DefaultStateName {
			if !IsValidName(s.Name) {
				return nil, fmt.Errorf("invalid tfstate name %q: must be a valid identifier (letters, digits, _, not starting with a digit)", s.Name)
			}
			prefix = s.Name
		}
		ldr := &stateLoader{ctx: ctx, loc: s.Location}
		funcs[prefix+"tfstate"] = func(addr string) (string, error) {
			return ldr.lookup(addr)
		}
		funcs[prefix+"tfstatef"] = func(format string, args ...any) (string, error) {
			return ldr.lookup(fmt.Sprintf(format, args...))
		}
	}

	tmpl, err := template.New("manifest").Funcs(funcs).Parse(string(manifest))
	if err != nil {
		return nil, fmt.Errorf("failed to parse manifest template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("failed to render manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// envFunc returns the value of the environment variable name (ecspresso-compatible
// {{ env "NAME" "default" }}). When the variable is unset or empty it returns default, and when
// default is omitted, the empty string.
func envFunc(name string, def ...string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if len(def) > 0 {
		return def[0]
	}
	return ""
}

// mustEnvFunc returns the value of the environment variable name (ecspresso-compatible
// {{ must_env "NAME" }}). It is an error when the variable is not defined. An empty value is
// accepted as long as the variable is "defined".
func mustEnvFunc(name string) (string, error) {
	if v, ok := os.LookupEnv(name); ok {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %q is not defined", name)
}

// jsonEscapeFunc returns the value escaped as the body of a JSON string
// (ecspresso-compatible {{ ... | json_escape }}). It does not add the surrounding ": the caller
// writes the quotes, as in '{"key": "{{ ... | json_escape }}"}'.
//
// Cloud Run manifests sometimes embed JSON as is in an annotation or in env[].value. When a
// value that comes from tfstate or an environment variable contains " or a newline, leaving it
// unescaped produces broken JSON/YAML.
func jsonEscapeFunc(v interface{}) (string, error) {
	text := fmt.Sprint(v)
	// json.Marshal does not treat invalid UTF-8 as an error; it replaces it with U+FFFD. Rather
	// than silently deploying a mangled value, refuse here (it can happen with bytes that come
	// from a secret).
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("json_escape: the value is not valid UTF-8")
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Do not turn & < > into a form like \u0026. As JSON it is equivalent, but a reader that takes
	// the value as is (an env[].value or annotation not re-parsed as JSON) sees it garbled.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(text); err != nil {
		return "", fmt.Errorf("failed to escape %v as JSON: %w", v, err)
	}
	// Encode adds a trailing newline and the surrounding ". Drop the newline and the quotes.
	encoded := strings.TrimRight(buf.String(), "\n")
	return encoded[1 : len(encoded)-1], nil
}

// stateLoader reads a state lazily and only once, and looks attributes up in it.
type stateLoader struct {
	ctx  context.Context
	loc  string
	once sync.Once
	st   *tfstate.TFState
	err  error
}

func (l *stateLoader) lookup(addr string) (string, error) {
	// ecspresso-compatible: replace ' in the address with " so no escaping is needed in YAML
	// (e.g. you can write aws_s3_bucket.main['id']). Same behaviour as tfstate-lookup's nameFunc.
	if strings.Contains(addr, "'") {
		addr = strings.ReplaceAll(addr, "'", "\"")
	}
	l.once.Do(func() {
		// A location with a scheme is read as a URL; anything else as a local file.
		if strings.Contains(l.loc, "://") {
			l.st, l.err = tfstate.ReadURL(l.ctx, l.loc)
		} else {
			l.st, l.err = tfstate.ReadFile(l.ctx, l.loc)
		}
	})
	if l.err != nil {
		return "", fmt.Errorf("failed to read tfstate %s: %w", l.loc, l.err)
	}

	obj, err := l.st.Lookup(addr)
	if err != nil {
		return "", fmt.Errorf("failed to look up %q in tfstate %s: %w", addr, l.loc, err)
	}
	if obj == nil || obj.Value == nil {
		return "", fmt.Errorf("%q not found in tfstate %s", addr, l.loc)
	}
	return obj.String(), nil
}
