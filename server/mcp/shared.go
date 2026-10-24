package mcp

import (
	"context"
	"encoding/json"
)

// The helpers below are the hardened schema and result handling MCP tools get, offered to the other tool kinds
// (HTTP tools) so there is one implementation of each to audit.

// CheckInputSchema reports whether raw is a usable tool input schema: an object schema within the size, depth,
// node and reference limits, resolvable without remote references.
func CheckInputSchema(ctx context.Context, raw []byte) error {
	_, err := validateSchemaContext(ctx, raw, defaultMaxSchemaBytes)
	return err
}

// ValidateArguments checks model-supplied arguments against a tool's input schema. It returns ErrInvalidArguments
// for arguments that are too large, malformed or do not satisfy the schema.
func ValidateArguments(ctx context.Context, schema, args []byte) error {
	if len(args) > defaultMaxArguments {
		return ErrInvalidArguments
	}
	instance, err := decodeJSON(args)
	if err != nil {
		return ErrInvalidArguments
	}
	resolved, err := validateSchemaContext(ctx, schema, defaultMaxSchemaBytes)
	if err != nil || resolved.Validate(schemaInstance(instance)) != nil {
		return ErrInvalidArguments
	}
	return nil
}

// SanitizeResult redacts secret-shaped keys and the credential from a tool result and bounds its size.
func SanitizeResult(raw []byte, credential string, maxBytes int) (json.RawMessage, error) {
	return sanitizeResult(raw, credential, maxBytes)
}

// CleanToolMessage makes a tool's message safe to hand to the model (control characters out, credential redacted, cut
// to a short length).
func CleanToolMessage(message, token string) string { return cleanToolMessage(message, token) }
