package sessionstore

import (
	"strings"
	"testing"
)

func TestSessionSchemaContainsDurableStateWithoutArtifactStorage(t *testing.T) {
	schema, err := migrations.ReadFile("migrations/001_sessions.sql")
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ToLower(string(schema))
	for _, required := range []string{
		"xworkmate_session_namespaces",
		"xworkmate_task_sessions",
		"xworkmate_session_events",
		"xworkmate_session_messages",
		"xworkmate_task_runs",
		"unique (session_id, client_request_id)",
	} {
		if !strings.Contains(normalized, required) {
			t.Fatalf("schema missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"artifact_content",
		"artifact_url",
		"artifact_path",
		"attachment",
		"base64",
		"working_directory",
		"tool_log",
		"provider_output",
	} {
		if strings.Contains(normalized, forbidden) {
			t.Fatalf("schema crosses artifact boundary with %q", forbidden)
		}
	}
}
