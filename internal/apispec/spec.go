package apispec

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

var tagDescriptions = map[string]string{
	"Health":               "Liveness, readiness and this contract.",
	"Auth":                 "Registration, email OTP verification, login, refresh rotation and password reset.",
	"Development":          "Local development helpers; not available in production.",
	"Account":              "Self-service profile, sessions, email change, data export and deletion.",
	"Categories":           "The active, database-managed taxonomy.",
	"Events":               "Event-centric news: synthesized summaries with citations, timelines and sources.",
	"Search":               "Hybrid lexical and semantic retrieval over events, articles and claims.",
	"Fact-checking":        "Claim-level verdicts with evidence, review status and correction history.",
	"Personalization":      "Preferences, interests, follows, feed and catch-up.",
	"Briefings":            "Text briefings and optional narration audio.",
	"Conversations":        "Source-grounded chat and event explanations.",
	"Notifications":        "Timezone-aware digest schedules, devices and delivery history.",
	"Admin: users":         "Role and status management.",
	"Admin: categories":    "Taxonomy administration and reclassification.",
	"Sources & publishers": "Publishers, content-rights permissions, endpoints, extraction profiles and health.",
	"Admin: operations":    "Durable jobs, dead letters and the audit trail.",
	"Admin: content":       "Article moderation and editorial event operations.",
	"Admin: fact-checking": "Review queue, evidence, decisions, publishing and narratives.",
	"Admin: AI":            "Providers, models, task routes, prompts, executions, evaluations and vector collections.",
	"Admin: policies":      "Editorial, ranking and notification policies and feature flags.",
}

var paramRe = regexp.MustCompile(`\{([A-Za-z]+)\}`)

var queryDocs = map[string]map[string]any{
	"lang":          {"description": "Response language (ro, ru, en); defaults to the user's summary language.", "schema": map[string]any{"type": "string", "enum": []string{"ro", "ru", "en"}}},
	"limit":         {"description": "Page size.", "schema": map[string]any{"type": "integer", "minimum": 1}},
	"cursor":        {"description": "Opaque cursor from next_cursor.", "schema": map[string]any{"type": "string"}},
	"since":         {"description": "RFC3339 timestamp.", "schema": map[string]any{"type": "string", "format": "date-time"}},
	"before":        {"description": "RFC3339 timestamp (exclusive upper bound).", "schema": map[string]any{"type": "string", "format": "date-time"}},
	"from":          {"description": "RFC3339 lower bound.", "schema": map[string]any{"type": "string", "format": "date-time"}},
	"to":            {"description": "RFC3339 upper bound.", "schema": map[string]any{"type": "string", "format": "date-time"}},
	"mode":          {"description": "Retrieval mode.", "schema": map[string]any{"type": "string", "enum": []string{"hybrid", "lexical", "semantic"}}},
	"level":         {"description": "Explanation depth.", "schema": map[string]any{"type": "string", "enum": []string{"short", "detailed", "background"}}},
	"days":          {"description": "Window in days.", "schema": map[string]any{"type": "integer"}},
	"hours":         {"description": "Window in hours.", "schema": map[string]any{"type": "integer"}},
	"rerank":        {"description": "Apply the reranking route.", "schema": map[string]any{"type": "boolean"}},
	"category_id":   {"description": "Category UUID.", "schema": map[string]any{"type": "string", "format": "uuid"}},
	"publisher_id":  {"description": "Publisher UUID.", "schema": map[string]any{"type": "string", "format": "uuid"}},
	"actor_id":      {"description": "Actor UUID.", "schema": map[string]any{"type": "string", "format": "uuid"}},
	"category_ids":  {"description": "Comma-separated category UUIDs.", "schema": map[string]any{"type": "string"}},
	"publisher_ids": {"description": "Comma-separated publisher UUIDs.", "schema": map[string]any{"type": "string"}},
	"types":         {"description": "Comma-separated doc types (event, article, claim).", "schema": map[string]any{"type": "string"}},
	"languages":     {"description": "Comma-separated languages.", "schema": map[string]any{"type": "string"}},
	"verdicts":      {"description": "Comma-separated verdicts.", "schema": map[string]any{"type": "string"}},
}

func queryParam(name string) map[string]any {
	p := map[string]any{"name": name, "in": "query", "required": false}
	if d, ok := queryDocs[name]; ok {
		for k, v := range d {
			p[k] = v
		}
	} else {
		p["schema"] = map[string]any{"type": "string"}
	}
	return p
}

func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '-' || r == '.' || r == '{' || r == '}' }) {
		if part == "v1" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

var problemResponses = map[string]string{
	"ValidationFailed": "Validation failed (field errors in `errors`)",
	"Unauthenticated":  "Missing or invalid access token",
	"Forbidden":        "Missing permission (`details.required_permission`), unverified or suspended account",
	"RefreshRejected":  "Invalid, expired or replayed refresh token",
	"NotFound":         "Resource not found (or not owned by the caller)",
	"Conflict":         "Version or state conflict",
	"RateLimited":      "Rate limited (see Retry-After)",
	"InternalError":    "Unexpected error (request_id included)",
}

func problem(name string) map[string]any {
	return map[string]any{"$ref": "#/components/responses/" + name}
}

func (r *registry) body(v any) map[string]any {
	var t reflect.Type
	switch x := v.(type) {
	case reflect.Type:
		t = x
	default:
		t = reflect.TypeOf(v)
	}
	return r.schemaOf(t)
}

// Build assembles the OpenAPI document.
func Build() map[string]any {
	reg := newRegistry()
	paths := map[string]map[string]any{}
	tags := map[string]bool{}
	for _, op := range Operations() {
		tags[op.Tag] = true
		o := map[string]any{"operationId": operationID(op.Method, op.Path), "tags": []string{op.Tag}, "summary": op.Summary}
		desc := []string{}
		switch op.Access {
		case Public:
			o["security"] = []any{}
			desc = append(desc, "Public: no credentials required.")
		case Refresh:
			o["security"] = []any{}
			desc = append(desc, "Requires a valid refresh token in the body.")
		case Dev:
			o["security"] = []any{}
			o["x-development-only"] = true
			desc = append(desc, "Development only: mounted only with MAIL_DRIVER=devsink outside production.")
		case Self:
			o["x-required-permissions"] = []string{"me:manage"}
			desc = append(desc, "Requires a verified, active human account (permission `me:manage`).")
		case Consumer:
			o["x-required-permissions"] = []string{"news:read"}
			desc = append(desc, "Requires a verified, active human account (permission `news:read`).")
		case Staff:
			o["x-required-permissions"] = op.Perms
			desc = append(desc, "Required permissions: `"+strings.Join(op.Perms, "`, `")+"`.")
		}
		if op.Notes != "" {
			desc = append(desc, op.Notes)
		}
		o["description"] = strings.Join(desc, "\n\n")

		var params []any
		for _, m := range paramRe.FindAllStringSubmatch(op.Path, -1) {
			s := map[string]any{"type": "string"}
			if m[1] == "id" || strings.HasSuffix(m[1], "Id") || m[1] == "iid" {
				s["format"] = "uuid"
			}
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": s})
		}
		for _, q := range op.Query {
			params = append(params, queryParam(q))
		}
		if len(params) > 0 {
			o["parameters"] = params
		}
		if op.Req != nil {
			o["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": reg.body(op.Req)}}}
		}

		status := op.Status
		if status == 0 {
			status = 200
		}
		resps := map[string]any{}
		switch {
		case status == 204:
			resps["204"] = map[string]any{"description": "No content"}
		case op.Resp == "audio":
			resps["200"] = map[string]any{"description": "Audio bytes", "content": map[string]any{"audio/mpeg": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}}
		case op.Resp == "json-file":
			resps["200"] = map[string]any{"description": "JSON file attachment", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": true}}}}
		case op.Resp == nil:
			resps[fmt.Sprint(status)] = map[string]any{"description": "OK", "content": map[string]any{"application/yaml": map[string]any{"schema": map[string]any{"type": "string"}}}}
		default:
			s := reg.body(op.Resp)
			if op.List {
				s = map[string]any{"type": "object", "required": []string{"items"}, "properties": map[string]any{
					"items": map[string]any{"type": "array", "items": s}, "next_cursor": map[string]any{"type": "string", "description": "Present when more results exist."}}}
			}
			d := "OK"
			if status == 202 {
				d = "Accepted for asynchronous processing"
			} else if status == 201 {
				d = "Created"
			}
			resps[fmt.Sprint(status)] = map[string]any{"description": d, "content": map[string]any{"application/json": map[string]any{"schema": s}}}
		}
		if op.Path == "/v1/briefings" && op.Method == "POST" {
			resps["200"] = map[string]any{"description": "An identical recent briefing was reused", "content": map[string]any{"application/json": map[string]any{"schema": reg.body(op.Resp)}}}
		}
		if op.Req != nil || len(op.Query) > 0 {
			resps["422"] = problem("ValidationFailed")
		}
		if op.Access != Public && op.Access != Dev && op.Access != Refresh {
			resps["401"] = problem("Unauthenticated")
			resps["403"] = problem("Forbidden")
		}
		if op.Access == Refresh {
			resps["401"] = problem("RefreshRejected")
		}
		if strings.Contains(op.Path, "{") {
			resps["404"] = problem("NotFound")
		}
		if op.Conflict {
			resps["409"] = problem("Conflict")
		}
		if strings.HasPrefix(op.Path, "/v1/auth/") || op.Path == "/v1/briefings" || strings.HasSuffix(op.Path, "/messages") || strings.HasSuffix(op.Path, "/audio") {
			resps["429"] = problem("RateLimited")
		}
		resps["500"] = problem("InternalError")
		o["responses"] = resps
		if paths[op.Path] == nil {
			paths[op.Path] = map[string]any{}
		}
		paths[op.Path][strings.ToLower(op.Method)] = o
	}

	reg.schemas["Problem"] = map[string]any{"type": "object", "required": []string{"type", "title", "status", "code"},
		"description": "RFC 9457 problem details.",
		"properties": map[string]any{
			"type": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "status": map[string]any{"type": "integer"},
			"code": map[string]any{"type": "string", "description": "Stable machine-readable error code."}, "detail": map[string]any{"type": "string"},
			"request_id": map[string]any{"type": "string"},
			"errors": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"field": map[string]any{"type": "string"}, "code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}}}},
			"details": map[string]any{"type": "object", "additionalProperties": true}}}

	responses := map[string]any{}
	for name, desc := range problemResponses {
		responses[name] = map[string]any{"description": desc, "content": map[string]any{"application/problem+json": map[string]any{
			"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}}}
	}
	var tagList []any
	var tagNames []string
	for t := range tags {
		tagNames = append(tagNames, t)
	}
	sort.Strings(tagNames)
	for _, t := range tagNames {
		tagList = append(tagList, map[string]any{"name": t, "description": tagDescriptions[t]})
	}
	pathsAny := map[string]any{}
	for k, v := range paths {
		pathsAny[k] = v
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{"title": "Moldova News Intelligence API", "version": "1.0.0",
			"description": "Backend API for event-centric news, evidence-based fact-checking, personalization, briefings, grounded chat and notifications. " +
				"All consumer endpoints require a verified account. Errors use application/problem+json. Times are RFC3339 UTC. " +
				"Generated from the Go types by `go run ./cmd/openapi-gen`; do not edit by hand."},
		"servers":  []any{map[string]any{"url": "http://localhost:8080", "description": "Local development"}},
		"security": []any{map[string]any{"bearerAuth": []string{}}},
		"tags":     tagList,
		"paths":    pathsAny,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT (EdDSA, short-lived)"},
				"serviceKey": map[string]any{"type": "apiKey", "in": "header", "name": "X-API-Key", "description": "Service accounts (machine identities) only."},
			},
			"schemas":   reg.schemas,
			"responses": responses,
		},
	}
}

// YAML renders the document deterministically.
func YAML() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# GENERATED by `go run ./cmd/openapi-gen` from internal/apispec — do not edit by hand.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(Build()); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}
