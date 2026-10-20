package contract

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed workunit.schema.json
var workUnitSchemaBytes []byte

//go:embed artifact.schema.json
var artifactSchemaBytes []byte

func WorkUnitSchemaBytes() []byte {
	return workUnitSchemaBytes
}

func ArtifactSchemaBytes() []byte {
	return artifactSchemaBytes
}

func ValidateWorkUnit(wu *WorkUnit) error {
	if wu == nil {
		return fmt.Errorf("work unit is nil")
	}
	compiler := jsonschema.NewCompiler()
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(workUnitSchemaBytes))
	if err != nil {
		return fmt.Errorf("failed to unmarshal workunit schema: %w", err)
	}
	if err := compiler.AddResource("workunit.schema.json", schemaDoc); err != nil {
		return fmt.Errorf("failed to add workunit schema resource: %w", err)
	}
	schema, err := compiler.Compile("workunit.schema.json")
	if err != nil {
		return fmt.Errorf("failed to compile workunit schema: %w", err)
	}

	rawJSON, err := json.Marshal(wu)
	if err != nil {
		return fmt.Errorf("failed to marshal workunit: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawJSON))
	if err != nil {
		return fmt.Errorf("failed to unmarshal workunit instance: %w", err)
	}

	if err := schema.Validate(inst); err != nil {
		return fmt.Errorf("work unit schema validation failed: %w", formatValidationError(wu, err))
	}
	return nil
}

func ValidateArtifact(art *Artifact) error {
	if art == nil {
		return fmt.Errorf("artifact is nil")
	}
	compiler := jsonschema.NewCompiler()
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(artifactSchemaBytes))
	if err != nil {
		return fmt.Errorf("failed to unmarshal artifact schema: %w", err)
	}
	if err := compiler.AddResource("artifact.schema.json", schemaDoc); err != nil {
		return fmt.Errorf("failed to add artifact schema resource: %w", err)
	}
	schema, err := compiler.Compile("artifact.schema.json")
	if err != nil {
		return fmt.Errorf("failed to compile artifact schema: %w", err)
	}

	rawJSON, err := json.Marshal(art)
	if err != nil {
		return fmt.Errorf("failed to marshal artifact: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawJSON))
	if err != nil {
		return fmt.Errorf("failed to unmarshal artifact instance: %w", err)
	}

	if err := schema.Validate(inst); err != nil {
		return fmt.Errorf("artifact schema validation failed: %w", err)
	}
	return nil
}

func formatValidationError(wu *WorkUnit, schemaErr error) error {
	var missing []string
	if strings.TrimSpace(wu.Objective) == "" {
		missing = append(missing, "objective")
	}
	if len(wu.Constraints) == 0 {
		missing = append(missing, "constraints")
	}
	if len(wu.Acceptance) == 0 {
		missing = append(missing, "acceptance")
	}
	if strings.TrimSpace(wu.Workflow) == "" {
		missing = append(missing, "workflow")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing or empty required fields [%s]: %w", strings.Join(missing, ", "), schemaErr)
	}
	return schemaErr
}
