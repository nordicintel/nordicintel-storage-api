package apidocs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nordicintel/nordicintel-storage-api/internal/httpapi"
	"github.com/nordicintel/nordicintel-storage-api/internal/jsonx"
)

func document(t *testing.T) map[string]any {
	t.Helper()
	var strict map[string]any
	if err := jsonx.DecodeStrict(Specification(), &strict); err != nil {
		t.Fatalf("the OpenAPI document is not strict, duplicate-free JSON: %v", err)
	}
	// DecodeStrict keeps numbers as json.Number so it can reject duplicates
	// without losing precision; the validator below compares plain values.
	var decoded map[string]any
	if err := json.Unmarshal(Specification(), &decoded); err != nil {
		t.Fatalf("the OpenAPI document is not valid JSON: %v", err)
	}
	return decoded
}

func TestSpecificationIsAnOpenAPI31Document(t *testing.T) {
	doc := document(t)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	info, ok := doc["info"].(map[string]any)
	if !ok {
		t.Fatal("the document has no info object")
	}
	for _, field := range []string{"title", "version"} {
		if value, ok := info[field].(string); !ok || value == "" {
			t.Fatalf("info.%s = %v, want a non-empty string", field, info[field])
		}
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		t.Fatal("the document has no paths object")
	}
	if _, ok := doc["components"].(map[string]any); !ok {
		t.Fatal("the document has no components object")
	}
}

func TestSpecificationReturnsADefensiveCopy(t *testing.T) {
	first := Specification()
	if len(first) == 0 {
		t.Fatal("Specification returned nothing")
	}
	first[0] = 'X'
	if second := Specification(); second[0] != '{' {
		t.Fatal("mutating a returned specification corrupted the embedded document")
	}
}

var httpMethods = map[string]string{
	"get": http.MethodGet, "put": http.MethodPut, "post": http.MethodPost,
	"delete": http.MethodDelete, "options": http.MethodOptions,
	"head": http.MethodHead, "patch": http.MethodPatch, "trace": http.MethodTrace,
}

// operations returns every documented path/method pair as "METHOD path".
func operations(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	found := make(map[string]map[string]any)
	for path, item := range doc["paths"].(map[string]any) {
		for field, value := range item.(map[string]any) {
			method, isMethod := httpMethods[field]
			if !isMethod {
				continue
			}
			operation, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%s %s is not an operation object", field, path)
			}
			found[method+" "+path] = operation
		}
	}
	return found
}

func TestDocumentedOperationsMatchTheRegisteredRoutes(t *testing.T) {
	documented := operations(t, document(t))
	registered := make(map[string]struct{})
	for _, route := range httpapi.Routes() {
		for _, method := range route.Methods {
			registered[method+" "+route.Pattern] = struct{}{}
		}
	}
	for operation := range documented {
		if _, ok := registered[operation]; !ok {
			t.Errorf("the document declares %s, which the router does not serve", operation)
		}
	}
	for operation := range registered {
		if _, ok := documented[operation]; !ok {
			t.Errorf("the router serves %s, which the document does not declare", operation)
		}
	}
	if t.Failed() {
		documentedNames := make([]string, 0, len(documented))
		for operation := range documented {
			documentedNames = append(documentedNames, operation)
		}
		sort.Strings(documentedNames)
		t.Logf("documented operations: %v", documentedNames)
	}
}

func TestEveryOperationHasAUniqueOperationIDAndResponses(t *testing.T) {
	seen := make(map[string]string)
	for name, operation := range operations(t, document(t)) {
		id, ok := operation["operationId"].(string)
		if !ok || id == "" {
			t.Fatalf("%s has no operationId", name)
		}
		if previous, duplicate := seen[id]; duplicate {
			t.Fatalf("operationId %q is used by both %s and %s", id, previous, name)
		}
		seen[id] = name
		responses, ok := operation["responses"].(map[string]any)
		if !ok || len(responses) == 0 {
			t.Fatalf("%s declares no responses", name)
		}
		for status := range responses {
			if !regexp.MustCompile(`^[1-5][0-9X]{2}$|^default$`).MatchString(status) {
				t.Fatalf("%s declares the invalid response key %q", name, status)
			}
		}
	}
}

func TestPathParametersAreDeclared(t *testing.T) {
	doc := document(t)
	components := doc["components"].(map[string]any)
	for path, item := range doc["paths"].(map[string]any) {
		templated := regexp.MustCompile(`\{([^}]+)\}`).FindAllStringSubmatch(path, -1)
		declared := make(map[string]struct{})
		for _, parameter := range parametersOf(t, item.(map[string]any), components) {
			if parameter["in"] == "path" {
				if parameter["required"] != true {
					t.Fatalf("path parameter %v on %s must be required", parameter["name"], path)
				}
				declared[parameter["name"].(string)] = struct{}{}
			}
		}
		for _, match := range templated {
			if _, ok := declared[match[1]]; !ok {
				t.Fatalf("path %s templates {%s} without declaring it", path, match[1])
			}
		}
	}
}

func parametersOf(t *testing.T, item, components map[string]any) []map[string]any {
	t.Helper()
	raw, ok := item["parameters"].([]any)
	if !ok {
		return nil
	}
	resolved := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		parameter := entry.(map[string]any)
		if ref, isRef := parameter["$ref"].(string); isRef {
			name := strings.TrimPrefix(ref, "#/components/parameters/")
			target, ok := components["parameters"].(map[string]any)[name].(map[string]any)
			if !ok {
				t.Fatalf("parameter reference %s does not resolve", ref)
			}
			parameter = target
		}
		resolved = append(resolved, parameter)
	}
	return resolved
}

func TestEveryReferenceResolves(t *testing.T) {
	doc := document(t)
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch value := node.(type) {
		case map[string]any:
			if ref, isRef := value["$ref"].(string); isRef {
				if resolve(doc, ref) == nil {
					t.Errorf("%s: reference %q does not resolve", path, ref)
				}
			}
			for key, child := range value {
				walk(child, path+"/"+key)
			}
		case []any:
			for i, child := range value {
				walk(child, fmt.Sprintf("%s/%d", path, i))
			}
		}
	}
	walk(doc, "")
}

func resolve(doc map[string]any, ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var node any = doc
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node, ok = object[segment]
		if !ok {
			return nil
		}
	}
	return node
}

func TestPublicAndPrivateOperationsDeclareTheRightSecurity(t *testing.T) {
	doc := document(t)
	global, ok := doc["security"].([]any)
	if !ok || len(global) != 1 {
		t.Fatalf("the document does not apply a single global security requirement: %v", doc["security"])
	}
	if _, ok := global[0].(map[string]any)["bearerAuth"]; !ok {
		t.Fatalf("the global security requirement is not bearerAuth: %v", global[0])
	}
	scheme := resolve(doc, "#/components/securitySchemes/bearerAuth")
	if scheme == nil {
		t.Fatal("bearerAuth is not defined")
	}
	if object := scheme.(map[string]any); object["type"] != "http" || object["scheme"] != "bearer" {
		t.Fatalf("bearerAuth = %v, want an HTTP bearer scheme", object)
	}

	public := make(map[string]bool)
	for _, route := range httpapi.Routes() {
		public[route.Pattern] = route.Public
	}
	for name, operation := range operations(t, doc) {
		path := name[strings.Index(name, " ")+1:]
		override, overridden := operation["security"]
		if public[path] {
			if !overridden {
				t.Errorf("%s is a public route but inherits the global bearer requirement", name)
				continue
			}
			if list, ok := override.([]any); !ok || len(list) != 0 {
				t.Errorf("%s is a public route but declares security %v", name, override)
			}
			continue
		}
		if overridden {
			t.Errorf("%s is a private route but overrides the global security requirement with %v", name, override)
		}
	}
}

func TestDocumentContainsNoCredentialsOrEnvironmentSpecificServers(t *testing.T) {
	doc := document(t)
	if servers, present := doc["servers"]; present {
		t.Fatalf("the document pins environment-specific servers: %v", servers)
	}
	body := strings.ToLower(string(Specification()))
	for _, forbidden := range []string{
		"api_read_write_token", "api_read_only_token", "database_url",
		"postgres://", "postgresql://", "localhost", "127.0.0.1",
		"amazonaws.com", "azurewebsites.net", "fly.dev", "onrender.com",
		"authorization: bearer ", "password",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the document contains %q", forbidden)
		}
	}
}

func TestEveryErrorResponseUsesTheErrorEnvelope(t *testing.T) {
	doc := document(t)
	for name, operation := range operations(t, doc) {
		if strings.HasSuffix(name, " /health") {
			continue // /health reports readiness with its own minimal object.
		}
		for status, response := range operation["responses"].(map[string]any) {
			if status < "400" || status == "default" {
				continue
			}
			resolved := response.(map[string]any)
			if ref, isRef := resolved["$ref"].(string); isRef {
				resolved = resolve(doc, ref).(map[string]any)
			}
			content, ok := resolved["content"].(map[string]any)
			if !ok {
				continue // 204 and redirect responses legitimately carry no body.
			}
			media, ok := content["application/json"].(map[string]any)
			if !ok {
				t.Fatalf("%s response %s does not describe a JSON body", name, status)
			}
			schema, ok := media["schema"].(map[string]any)
			if !ok || schema["$ref"] != "#/components/schemas/Error" {
				t.Fatalf("%s response %s does not use the Error envelope: %v", name, status, media["schema"])
			}
		}
	}
}

func TestEveryDocumentedErrorCodeIsStable(t *testing.T) {
	// The contract fixes this list; a new code must be added deliberately.
	stable := map[string]struct{}{
		"invalid_json": {}, "invalid_query": {}, "invalid_path_code": {},
		"unauthorized": {}, "forbidden": {}, "not_found": {},
		"method_not_allowed": {}, "dataset_exists": {}, "request_too_large": {},
		"unsupported_media_type": {}, "validation_failed": {},
		"cell_limit_exceeded": {}, "internal_error": {}, "service_unavailable": {},
	}
	for _, match := range regexp.MustCompile(`"code"\s*:\s*"([a-z_]+)"`).FindAllStringSubmatch(string(Specification()), -1) {
		if _, ok := stable[match[1]]; !ok {
			t.Fatalf("the document uses the unlisted error code %q", match[1])
		}
	}
}

// -------------------------------------------------------------- examples ---

// TestEveryExampleValidatesAgainstItsSchema walks every example in the document
// and checks it against the sibling schema, so the published examples cannot
// drift away from the contract they illustrate.
func TestEveryExampleValidatesAgainstItsSchema(t *testing.T) {
	doc := document(t)
	checked := 0
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch value := node.(type) {
		case map[string]any:
			schema, hasSchema := value["schema"].(map[string]any)
			if example, hasExample := value["example"]; hasExample && hasSchema {
				if err := validate(doc, schema, example, path); err != nil {
					t.Errorf("%s: example does not match its schema: %v", path, err)
				}
				checked++
			}
			if examples, hasExamples := value["examples"].(map[string]any); hasExamples && hasSchema {
				for name, wrapper := range examples {
					object, ok := wrapper.(map[string]any)
					if !ok {
						continue
					}
					if example, present := object["value"]; present {
						if err := validate(doc, schema, example, path+"/examples/"+name); err != nil {
							t.Errorf("%s/examples/%s: example does not match its schema: %v", path, name, err)
						}
						checked++
					}
				}
			}
			for key, child := range value {
				walk(child, path+"/"+key)
			}
		case []any:
			for i, child := range value {
				walk(child, fmt.Sprintf("%s/%d", path, i))
			}
		}
	}
	walk(doc, "")
	if checked == 0 {
		t.Fatal("the document contains no examples to validate")
	}
	t.Logf("validated %d examples", checked)
}

// TestContractExamplesValidateAgainstTheirSchemas checks the concrete payloads
// printed in the Markdown contract against the published schemas, so the two
// descriptions of the same API cannot disagree.
func TestContractExamplesValidateAgainstTheirSchemas(t *testing.T) {
	doc := document(t)
	const summary = `{"provider_code":"SCB","dataset_code":"Population","source_stamp":{"etag":"abc"},` +
		`"cell_count":4,"valued_cell_count":3,"null_cell_count":1,"updated_at":"2026-09-01T12:00:00Z"}`
	cases := []struct {
		name   string
		schema string
		body   string
	}{
		{"provider list", "#/components/schemas/Provider", `{"provider_code":"SCB","dataset_count":2}`},
		{"dataset summary", "#/components/schemas/DatasetSummary", summary},
		{"summary with a null stamp", "#/components/schemas/DatasetSummary",
			strings.Replace(summary, `{"etag":"abc"}`, `null`, 1)},
		{"mutation result", "#/components/schemas/MutationResult",
			`{"result":"created","dataset":` + summary + `}`},
		{"structure", "#/components/schemas/Structure",
			`{"id":["sex","year"],"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}}}`},
		{"dense replacement", "#/components/schemas/Replacement",
			`{"replace":false,"source_stamp":{"etag":"abc"},"id":["sex","year"],` +
				`"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}},` +
				`"value":[10.5,null,null,null],"text":[null,null,null,"confidential"],"status":[null,null,null,"c"]}`},
		{"sparse replacement with scalar status", "#/components/schemas/Replacement",
			`{"source_stamp":null,"id":["sex"],"dimension":{"sex":{"index":{"M":0}}},` +
				`"value":{"0":1.5},"status":"c"}`},
		{"sparse data response", "#/components/schemas/DataResponse",
			`{"provider_code":"SCB","dataset_code":"Population","source_stamp":{"etag":"abc"},` +
				`"cell_count":4,"valued_cell_count":2,"null_cell_count":2,"updated_at":"2026-09-01T12:00:00Z",` +
				`"id":["sex","year"],"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}},` +
				`"value":{"0":10.5},"text":{"3":"confidential"},"status":{"3":"c"}}`},
		{"dense data response", "#/components/schemas/DataResponse",
			`{"provider_code":"SCB","dataset_code":"Population","source_stamp":null,` +
				`"cell_count":2,"valued_cell_count":1,"null_cell_count":1,"updated_at":"2026-09-01T12:00:00Z",` +
				`"id":["sex"],"dimension":{"sex":{"index":{"M":0,"F":1}}},"value":[10.5,null],"status":"c"}`},
		{"structure response", "#/components/schemas/StructureResponse",
			`{"provider_code":"SCB","dataset_code":"Population","source_stamp":null,` +
				`"cell_count":2,"valued_cell_count":0,"null_cell_count":2,"updated_at":"2026-09-01T12:00:00Z",` +
				`"id":["sex"],"dimension":{"sex":{"index":{"M":0,"F":1}}}}`},
		{"error envelope", "#/components/schemas/Error",
			`{"error":{"code":"validation_failed","message":"dimension indexes must be contiguous","request_id":"abc"}}`},
		{"healthy", "#/components/schemas/Health", `{"status":"ok"}`},
		{"unavailable", "#/components/schemas/Health", `{"status":"unavailable"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.body), &value); err != nil {
				t.Fatalf("the fixture is not valid JSON: %v", err)
			}
			schema := resolve(doc, tc.schema)
			if schema == nil {
				t.Fatalf("schema %s does not exist", tc.schema)
			}
			if err := validate(doc, schema.(map[string]any), value, tc.schema); err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestSchemasRejectContractViolations(t *testing.T) {
	doc := document(t)
	cases := []struct {
		name   string
		schema string
		body   string
	}{
		{"summary missing a count", "#/components/schemas/DatasetSummary",
			`{"provider_code":"SCB","dataset_code":"P","source_stamp":null,"cell_count":1,"valued_cell_count":0,"updated_at":"2026-09-01T12:00:00Z"}`},
		{"summary with an unknown field", "#/components/schemas/DatasetSummary",
			`{"provider_code":"SCB","dataset_code":"P","source_stamp":null,"cell_count":1,` +
				`"valued_cell_count":0,"null_cell_count":1,"updated_at":"2026-09-01T12:00:00Z","extra":1}`},
		{"cell count over the ceiling", "#/components/schemas/DatasetSummary",
			`{"provider_code":"SCB","dataset_code":"P","source_stamp":null,"cell_count":1000001,` +
				`"valued_cell_count":0,"null_cell_count":1000001,"updated_at":"2026-09-01T12:00:00Z"}`},
		{"structure without dimensions", "#/components/schemas/Structure", `{"id":[],"dimension":{}}`},
		{"replacement without a value channel", "#/components/schemas/Replacement",
			`{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}}}`},
		{"replacement without a source stamp", "#/components/schemas/Replacement",
			`{"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`},
		{"unknown health status", "#/components/schemas/Health", `{"status":"degraded"}`},
		{"unknown mutation result", "#/components/schemas/MutationResult",
			`{"result":"updated","dataset":{"provider_code":"SCB","dataset_code":"P","source_stamp":null,` +
				`"cell_count":1,"valued_cell_count":0,"null_cell_count":1,"updated_at":"2026-09-01T12:00:00Z"}}`},
		{"error without a request id", "#/components/schemas/Error",
			`{"error":{"code":"not_found","message":"missing"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.body), &value); err != nil {
				t.Fatalf("the fixture is not valid JSON: %v", err)
			}
			schema := resolve(doc, tc.schema).(map[string]any)
			if err := validate(doc, schema, value, tc.schema); err == nil {
				t.Fatalf("%s accepted a contract violation", tc.schema)
			}
		})
	}
}

// validate checks a value against the subset of JSON Schema the contract uses:
// $ref, allOf, oneOf, type (including type unions), enum, required, properties,
// additionalProperties, items, and the numeric, array, and object bounds.
func validate(doc map[string]any, schema map[string]any, value any, path string) error {
	if ref, isRef := schema["$ref"].(string); isRef {
		target := resolve(doc, ref)
		if target == nil {
			return fmt.Errorf("%s: reference %q does not resolve", path, ref)
		}
		return validate(doc, target.(map[string]any), value, path)
	}
	if all, present := schema["allOf"].([]any); present {
		for i, entry := range all {
			if err := validate(doc, entry.(map[string]any), value, fmt.Sprintf("%s/allOf/%d", path, i)); err != nil {
				return err
			}
		}
	}
	if one, present := schema["oneOf"].([]any); present {
		matches := 0
		for _, entry := range one {
			if validate(doc, entry.(map[string]any), value, path) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: %d of %d oneOf branches matched", path, matches, len(one))
		}
	}
	if types, present := schema["type"]; present {
		if !matchesType(types, value) {
			return fmt.Errorf("%s: value %v does not have type %v", path, value, types)
		}
	}
	if enum, present := schema["enum"].([]any); present {
		found := false
		for _, allowed := range enum {
			if fmt.Sprint(allowed) == fmt.Sprint(value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: value %v is not one of %v", path, value, enum)
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		if required, present := schema["required"].([]any); present {
			for _, name := range required {
				if _, ok := typed[name.(string)]; !ok {
					return fmt.Errorf("%s: required property %v is missing", path, name)
				}
			}
		}
		if err := checkCount(path, "properties", len(typed),
			schema["minProperties"], schema["maxProperties"]); err != nil {
			return err
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, child := range typed {
			if property, declared := properties[name]; declared {
				if err := validate(doc, property.(map[string]any), child, path+"/"+name); err != nil {
					return err
				}
				continue
			}
			switch additional := schema["additionalProperties"].(type) {
			case bool:
				if !additional {
					return fmt.Errorf("%s: property %q is not allowed", path, name)
				}
			case map[string]any:
				if err := validate(doc, additional, child, path+"/"+name); err != nil {
					return err
				}
			}
		}
	case []any:
		if err := checkCount(path, "items", len(typed), schema["minItems"], schema["maxItems"]); err != nil {
			return err
		}
		if items, present := schema["items"].(map[string]any); present {
			for i, child := range typed {
				if err := validate(doc, items, child, fmt.Sprintf("%s/%d", path, i)); err != nil {
					return err
				}
			}
		}
	case float64:
		if minimum, present := schema["minimum"].(float64); present && typed < minimum {
			return fmt.Errorf("%s: %v is below the minimum %v", path, typed, minimum)
		}
		if maximum, present := schema["maximum"].(float64); present && typed > maximum {
			return fmt.Errorf("%s: %v is above the maximum %v", path, typed, maximum)
		}
	case string:
		if minimum, present := schema["minLength"].(float64); present && float64(len(typed)) < minimum {
			return fmt.Errorf("%s: %q is shorter than %v", path, typed, minimum)
		}
		if maximum, present := schema["maxLength"].(float64); present && float64(len(typed)) > maximum {
			return fmt.Errorf("%s: %q is longer than %v", path, typed, maximum)
		}
	}
	return nil
}

func checkCount(path, what string, count int, minimum, maximum any) error {
	if bound, present := minimum.(float64); present && float64(count) < bound {
		return fmt.Errorf("%s: %d %s is fewer than %v", path, count, what, bound)
	}
	if bound, present := maximum.(float64); present && float64(count) > bound {
		return fmt.Errorf("%s: %d %s is more than %v", path, count, what, bound)
	}
	return nil
}

func matchesType(declared any, value any) bool {
	switch typed := declared.(type) {
	case string:
		return matchesSingleType(typed, value)
	case []any:
		for _, entry := range typed {
			if matchesSingleType(entry.(string), value) {
				return true
			}
		}
		return false
	}
	return true
}

func matchesSingleType(name string, value any) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	case "null":
		return value == nil
	}
	return true
}

// TestResponseSchemasRepeatTheSummaryFieldsExactly guards the flat response
// schemas against drifting away from DatasetSummary. The schemas are flat
// rather than composed with allOf because "additionalProperties": false only
// sees properties declared in the same schema object, so a composed closed
// object would reject every valid response.
func TestResponseSchemasRepeatTheSummaryFieldsExactly(t *testing.T) {
	doc := document(t)
	summary := resolve(doc, "#/components/schemas/DatasetSummary").(map[string]any)
	reference := summary["properties"].(map[string]any)
	for _, name := range []string{"StructureResponse", "DataResponse"} {
		schema := resolve(doc, "#/components/schemas/"+name).(map[string]any)
		properties := schema["properties"].(map[string]any)
		for field, want := range reference {
			got, present := properties[field]
			if !present {
				t.Fatalf("%s is missing the summary field %q", name, field)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s.%s = %v, want the DatasetSummary definition %v", name, field, got, want)
			}
		}
		required := make(map[string]struct{})
		for _, field := range schema["required"].([]any) {
			required[field.(string)] = struct{}{}
		}
		for _, field := range summary["required"].([]any) {
			if _, ok := required[field.(string)]; !ok {
				t.Fatalf("%s does not require the summary field %v", name, field)
			}
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s is not a closed object", name)
		}
	}
}
