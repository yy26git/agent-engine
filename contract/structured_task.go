package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// StructuredTask is data supplied to a locally approved report producer. It
// carries no executable, model override, filesystem grant or review authority.
type StructuredTask struct {
	Function   string `json:"function"`
	SchemaName string `json:"schema_name"`
	SchemaJSON string `json:"schema_json"`
	System     string `json:"system"`
	User       string `json:"user"`
}

func (t *StructuredTask) Digest() string {
	h := sha256.New()
	for _, s := range []string{t.Function, t.SchemaName, t.SchemaJSON, t.System, t.User} {
		fmt.Fprintf(h, "%d:", len([]byte(s)))
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func (t *StructuredTask) Validate() error {
	if t == nil {
		return fmt.Errorf("structured task is required")
	}
	switch t.Function {
	case "semantic-evaluation", "report-analysis":
	default:
		return fmt.Errorf("report-only supports semantic-evaluation and report-analysis")
	}
	if strings.TrimSpace(t.SchemaName) == "" || strings.TrimSpace(t.User) == "" || len(t.SchemaName) > 128 || len(t.SchemaJSON)+len(t.System)+len(t.User) > 512*1024 {
		return fmt.Errorf("structured task requires a named schema and input within 512 KiB")
	}
	_, err := t.compile()
	return err
}

// External references must never turn admission into a network/filesystem read.
type noSchemaLoader struct{}

func (noSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema resource refused: %s", url)
}
func (t *StructuredTask) compile() (*jsonschema.Schema, error) {
	var doc any
	if err := json.Unmarshal([]byte(t.SchemaJSON), &doc); err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noSchemaLoader{})
	const name = "https://agent-engine.invalid/report-schema.json"
	if err := c.AddResource(name, doc); err != nil {
		return nil, err
	}
	return c.Compile(name)
}
func (t *StructuredTask) ValidateOutput(raw string) error {
	if len(raw) > 2*1024*1024 {
		return fmt.Errorf("report exceeds 2 MiB")
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return err
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("report output must be a JSON object")
	}
	s, err := t.compile()
	if err != nil {
		return err
	}
	return s.Validate(value)
}
