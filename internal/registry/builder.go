package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/output"
	"github.com/airbytehq/airbyte-agent-cli/internal/telemetry"
	"github.com/spf13/cobra"
)

// osExit is overridable for tests. In production it flushes the
// in-flight telemetry event before terminating so we don't drop events
// on validation / API-error exit paths.
var osExit = func(code int) {
	emitPendingEvent()
	os.Exit(code)
}

// tracker is the package-level telemetry emitter. main.go installs it
// via SetTracker before the registry is built; tests leave it nil.
// All emission goes through *Tracker's nil-safe methods.
var tracker *telemetry.Tracker

// SetTracker installs the tracker the registry emits to. Safe to call
// once at startup with a nil value to disable emission.
func SetTracker(t *telemetry.Tracker) {
	tracker = t
}

// In-flight event state for the currently-running RunE. The CLI runs
// one command per invocation, so a single package-level slot is safe.
// Set at the top of RunE; emitted either by the deferred cleanup on
// normal return or by osExit on early-exit paths. emitPendingEvent
// guards against double-emission.
var (
	currentEvent      *telemetry.CommandEvent
	currentEventStart time.Time
	eventEmitted      bool
)

// beginTrackedCommand initializes the in-flight event for a RunE that
// is about to execute. Each call resets the package-level slot — the
// CLI runs one command per invocation, so the prior slot (if any) is
// stale.
func beginTrackedCommand(cmd *cobra.Command, op *Operation) {
	currentEvent = &telemetry.CommandEvent{Command: trackedCommandName(cmd, op)}
	currentEventStart = time.Now()
	eventEmitted = false
}

// trackedCommandName returns the leaf command path, e.g.
// "connectors execute". Uses cmd.Parent() so resource-name renames stay
// reflected without the registry having to plumb the name through.
func trackedCommandName(cmd *cobra.Command, op *Operation) string {
	if cmd != nil && cmd.Parent() != nil && cmd.Parent().Name() != "" {
		return cmd.Parent().Name() + " " + op.Name
	}
	return op.Name
}

// annotateEventFromParams pulls entity/action out of the params map for
// `connectors execute` (the one tracked command where these have
// meaningful operation-identifier semantics). For other commands the
// keys are absent and this is a no-op.
func annotateEventFromParams(params map[string]any) {
	if currentEvent == nil {
		return
	}
	if v, ok := params["entity"].(string); ok {
		currentEvent.Entity = v
	}
	if v, ok := params["action"].(string); ok {
		currentEvent.Action = v
	}
}

// emitPendingEvent flushes the in-flight event to the tracker. Idempotent
// per invocation — repeated calls within the same RunE no-op.
func emitPendingEvent() {
	if eventEmitted || currentEvent == nil {
		return
	}
	eventEmitted = true
	currentEvent.DurationMs = time.Since(currentEventStart).Milliseconds()
	if currentEvent.ErrorType == "" {
		currentEvent.Success = true
	}
	tracker.TrackCommand(*currentEvent)
	tracker.Flush()
}

type flagAccessor interface {
	GetOutput() string
	GetFields() []string
}

func Build(rootCmd *cobra.Command, c *client.Client, flags flagAccessor) {
	for _, res := range All() {
		resCmd := &cobra.Command{
			Use:   res.Name(),
			Short: res.Description(),
			Args:  UnknownSubcommandArgs,
			Run: func(cmd *cobra.Command, args []string) {
				_ = cmd.Help()
			},
		}

		for i := range res.Operations() {
			op := res.Operations()[i]
			opCmd := buildOperationCmd(&op, c, flags)
			resCmd.AddCommand(opCmd)
		}

		rootCmd.AddCommand(resCmd)
	}
}

// paramBinding holds the flag-name and the bound pointer for a single schema
// parameter. Only the field matching `kind` is populated.
type paramBinding struct {
	flagName string
	kind     string
	strVal   *string
	boolVal  *bool
	intVal   *int
	floatVal *float64
	sliceVal *[]string
}

var reservedFlagNames = map[string]bool{
	"help":    true,
	"json":    true,
	"output":  true,
	"verbose": true,
}

// flagNameFor maps a snake_case schema key to a kebab-case CLI flag.
func flagNameFor(schemaKey string) string {
	return strings.ReplaceAll(schemaKey, "_", "-")
}

func buildOperationCmd(op *Operation, c *client.Client, flags flagAccessor) *cobra.Command {
	var jsonInput string
	bindings := map[string]*paramBinding{}
	var buildErrors []string

	cmd := &cobra.Command{
		Use:   op.Name,
		Short: op.Description,
		RunE: func(cmd *cobra.Command, args []string) error {
			beginTrackedCommand(cmd, op)
			defer emitPendingEvent()

			if len(buildErrors) > 0 {
				return handleRunError(client.NewValidationError(
					"operation schema has invalid parameter flags: "+strings.Join(buildErrors, "; "),
					"rename conflicting schema parameters",
				))
			}

			params, err := collectParams(cmd, jsonInput, bindings, op.Schema)
			if err != nil {
				return err
			}

			annotateEventFromParams(params)

			ctx := context.Background()

			if c == nil && !op.Hooks.AllowUnauthenticated {
				return handleRunError(&client.APIError{
					Type:       "auth_error",
					Message:    "no credentials configured",
					StatusCode: 401,
					Hint:       "run 'airbyte-agent login' to configure credentials interactively, set AIRBYTE_CLIENT_ID/AIRBYTE_CLIENT_SECRET/AIRBYTE_ORGANIZATION_ID environment variables, or populate ~/.airbyte-agent/settings.json",
				})
			}

			if op.Hooks.Interactive != nil {
				result, err := op.Hooks.Interactive(ctx, c, params)
				if err != nil {
					return handleRunError(err)
				}
				return writeResult(result, flags)
			}

			if op.Hooks.PreRun != nil {
				params, err = op.Hooks.PreRun(ctx, c, params)
				if err != nil {
					return handleRunError(err)
				}
			}

			result, err := op.Run(ctx, c, params)
			if err != nil {
				return handleRunError(err)
			}

			return writeResult(result, flags)
		},
		SilenceUsage: true,
	}

	cmd.Flags().StringVar(&jsonInput, "json", "", "Input parameters as JSON (or @filename to read from file). Cannot be combined with parameter flags.")

	keys := make([]string, 0, len(op.Schema.Params))
	for k := range op.Schema.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ps := op.Schema.Params[key]
		flagName := flagNameFor(key)
		desc := ps.Description
		if ps.Required {
			desc += " (required)"
		}
		if !supportsFlagType(ps.Type) {
			// "object" or unknown types have no flag form. Caller must use --json.
			continue
		}
		b := &paramBinding{flagName: flagName, kind: ps.Type}
		if reservedFlagNames[flagName] {
			buildErrors = append(buildErrors, fmt.Sprintf("%q maps to reserved flag --%s", key, flagName))
			continue
		}
		if existingKey, ok := flagKeyByName(bindings, flagName); ok {
			buildErrors = append(buildErrors, fmt.Sprintf("%q and %q both map to --%s", existingKey, key, flagName))
			continue
		}
		switch ps.Type {
		case "string":
			b.strVal = new(string)
			cmd.Flags().StringVar(b.strVal, flagName, "", desc)
		case "bool", "boolean":
			b.boolVal = new(bool)
			cmd.Flags().BoolVar(b.boolVal, flagName, false, desc)
		case "int", "integer":
			b.intVal = new(int)
			cmd.Flags().IntVar(b.intVal, flagName, 0, desc)
		case "number":
			b.floatVal = new(float64)
			cmd.Flags().Float64Var(b.floatVal, flagName, 0, desc)
		case "array":
			b.sliceVal = new([]string)
			cmd.Flags().StringSliceVar(b.sliceVal, flagName, nil, desc+" (comma-separated, or repeat the flag)")
		case "object":
			b.strVal = new(string)
			cmd.Flags().StringVar(b.strVal, flagName, "", desc+" (JSON object)")
		}
		bindings[key] = b
	}

	return cmd
}

func supportsFlagType(typ string) bool {
	switch typ {
	case "string", "bool", "boolean", "int", "integer", "number", "array", "object":
		return true
	default:
		return false
	}
}

func flagKeyByName(bindings map[string]*paramBinding, flagName string) (string, bool) {
	for key, b := range bindings {
		if b.flagName == flagName {
			return key, true
		}
	}
	return "", false
}

// collectParams resolves the operation's parameters from either --json or the
// per-parameter flags, enforcing that the two modes are mutually exclusive.
func collectParams(cmd *cobra.Command, jsonInput string, bindings map[string]*paramBinding, schema OperationSchema) (map[string]any, error) {
	jsonSet := cmd.Flags().Changed("json")

	var setParamFlags []string
	for _, b := range bindings {
		if cmd.Flags().Changed(b.flagName) {
			setParamFlags = append(setParamFlags, "--"+b.flagName)
		}
	}
	sort.Strings(setParamFlags)

	if jsonSet && len(setParamFlags) > 0 {
		writeStderrJSON(map[string]any{
			"type":    "validation_error",
			"message": fmt.Sprintf("--json cannot be combined with parameter flags (%s)", strings.Join(setParamFlags, ", ")),
			"hint":    "pass parameters either as --json or as individual flags, not both",
		})
		osExit(client.ExitValidation)
		return nil, fmt.Errorf("validation error")
	}

	params := map[string]any{}

	if jsonSet {
		raw, err := resolveJSONInput(jsonInput)
		if err != nil {
			writeStderrError("input_error", err.Error())
			osExit(client.ExitValidation)
			return nil, fmt.Errorf("input error")
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			writeStderrError("input_error", fmt.Sprintf("invalid JSON: %s", err.Error()))
			osExit(client.ExitValidation)
			return nil, fmt.Errorf("invalid JSON")
		}
	} else {
		for key, b := range bindings {
			if !cmd.Flags().Changed(b.flagName) {
				continue
			}
			switch b.kind {
			case "string":
				params[key] = *b.strVal
			case "bool", "boolean":
				params[key] = *b.boolVal
			case "int", "integer":
				params[key] = *b.intVal
			case "number":
				params[key] = *b.floatVal
			case "array":
				arr := make([]any, len(*b.sliceVal))
				for i, v := range *b.sliceVal {
					arr[i] = v
				}
				params[key] = arr
			case "object":
				var obj any
				if err := json.Unmarshal([]byte(*b.strVal), &obj); err != nil {
					writeStderrJSON(map[string]any{
						"type":    "validation_error",
						"message": fmt.Sprintf("--%s expected a JSON object: %s", b.flagName, err.Error()),
					})
					osExit(client.ExitValidation)
					return nil, fmt.Errorf("invalid JSON in --%s", b.flagName)
				}
				params[key] = obj
			}
		}
	}

	if err := validateParams(params, schema); err != nil {
		return nil, err
	}

	return params, nil
}

func resolveJSONInput(input string) ([]byte, error) {
	if strings.HasPrefix(input, "@") {
		filename := input[1:]
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("reading file %s: %w", filename, err)
		}
		return data, nil
	}
	return []byte(input), nil
}

func validateParams(params map[string]any, schema OperationSchema) error {
	fields := make(map[string]string)
	for name := range params {
		if _, ok := schema.Params[name]; !ok {
			fields[name] = "unknown parameter"
		}
	}

	for name, ps := range schema.Params {
		value, ok := params[name]
		if !ok {
			if ps.Required {
				fields[name] = "required"
			}
			continue
		}

		if ps.Required && isEmptyValue(value) {
			fields[name] = "required"
			continue
		}
		if msg := validateParamType(value, ps.Type); msg != "" {
			fields[name] = msg
		}
	}

	if len(fields) > 0 {
		errPayload := map[string]any{
			"type":   "validation_error",
			"fields": fields,
			"hint":   "run `airbyte-agent schema <resource> <operation>` to see the expected parameter schema",
		}
		writeStderrJSON(errPayload)
		osExit(client.ExitValidation)
		return fmt.Errorf("validation error")
	}

	return nil
}

func isEmptyValue(value any) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case []string:
		return len(v) == 0
	default:
		return value == nil
	}
}

func validateParamType(value any, typ string) string {
	switch typ {
	case "string":
		if _, ok := value.(string); !ok {
			return "expected string"
		}
	case "bool", "boolean":
		if _, ok := value.(bool); !ok {
			return "expected boolean"
		}
	case "int", "integer":
		switch v := value.(type) {
		case int:
		case int64:
		case float64:
			if v != float64(int64(v)) {
				return "expected integer"
			}
		default:
			return "expected integer"
		}
	case "number":
		switch value.(type) {
		case int, int64, float64:
		default:
			return "expected number"
		}
	case "array":
		switch value.(type) {
		case []any, []string:
		default:
			return "expected array"
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return "expected object"
		}
	}
	return ""
}

func handleRunError(err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		errPayload := map[string]any{
			"type":        apiErr.Type,
			"message":     apiErr.Message,
			"status_code": apiErr.StatusCode,
			"retryable":   apiErr.Retryable,
		}
		if apiErr.Detail != nil {
			errPayload["detail"] = apiErr.Detail
		}
		if apiErr.Hint != "" {
			errPayload["hint"] = apiErr.Hint
		}
		writeStderrJSON(errPayload)
		osExit(apiErr.ExitCode())
		return err
	}
	writeStderrError("error", err.Error())
	osExit(client.ExitGeneral)
	return err
}

func writeResult(result any, flags flagAccessor) error {
	if fields := flags.GetFields(); len(fields) > 0 {
		filtered, matched, err := output.FilterWithMatch(result, fields)
		if err != nil {
			return handleRunError(fmt.Errorf("filtering --fields: %w", err))
		}
		if !matched {
			validationErr := client.NewValidationError(
				fmt.Sprintf("none of the requested --fields paths matched the response: %s", strings.Join(fields, ", ")),
				"the operation already completed; inspect detail.response and correct --fields for future calls; do not re-run a write action solely to repair its projection",
			)
			if detail, err := json.Marshal(map[string]any{
				"operation_completed": true,
				"response":            result,
			}); err == nil {
				validationErr.Detail = detail
			}
			return handleRunError(validationErr)
		}
		result = filtered
	}
	return output.Write(result, flags.GetOutput())
}

func writeStderrError(errType, message string) {
	writeStderrJSON(map[string]any{
		"type":    errType,
		"message": message,
	})
}

func writeStderrJSON(payload map[string]any) {
	captureErrorIntoEvent(payload)
	output.WriteError(payload)
}

// captureErrorIntoEvent pulls error_type + status_code from an
// about-to-be-emitted stderr payload into the in-flight event so the
// telemetry side records the same failure shape the user sees, without
// each call site having to remember.
func captureErrorIntoEvent(payload map[string]any) {
	if currentEvent == nil {
		return
	}
	if currentEvent.ErrorType == "" {
		if s, ok := payload["type"].(string); ok {
			currentEvent.ErrorType = s
		}
	}
	if currentEvent.StatusCode == 0 {
		switch v := payload["status_code"].(type) {
		case int:
			currentEvent.StatusCode = v
		case int64:
			currentEvent.StatusCode = int(v)
		case float64:
			currentEvent.StatusCode = int(v)
		}
	}
}
