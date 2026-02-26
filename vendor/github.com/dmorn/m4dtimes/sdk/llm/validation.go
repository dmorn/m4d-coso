package llm

import (
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func ValidateToolArgs(tool ToolDef, args json.RawMessage) error {
	compiler := jsonschema.NewCompiler()

	var schemaDoc any
	if err := json.Unmarshal(tool.Parameters, &schemaDoc); err != nil {
		return fmt.Errorf("invalid JSON schema for tool %q: %w", tool.Name, err)
	}
	if err := compiler.AddResource("tool-schema.json", schemaDoc); err != nil {
		return fmt.Errorf("invalid JSON schema for tool %q: %w", tool.Name, err)
	}

	schema, err := compiler.Compile("tool-schema.json")
	if err != nil {
		return fmt.Errorf("failed to compile JSON schema for tool %q: %w", tool.Name, err)
	}

	var value any
	if err := json.Unmarshal(args, &value); err != nil {
		return fmt.Errorf("invalid JSON arguments for tool %q: %w", tool.Name, err)
	}

	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("tool arguments validation failed for %q: %w", tool.Name, err)
	}
	return nil
}
