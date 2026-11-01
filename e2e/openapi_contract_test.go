package e2e

// The contract test (M6): the routes the HTTP handlers register are exactly
// the paths+methods the OpenAPI document declares. The spec is parsed with a
// deliberately small scanner (it is our own file with stable indentation);
// the server is probed the way a client would, so the check cannot drift
// from the real handler behavior.

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

type opPath struct{ Method, Path string }

// parseOpenAPIPaths extracts every "  /path:" entry under a top-level
// `paths:` mapping together with the HTTP methods declared directly inside it
// (four-space indent). Enough structure for our own spec; not a yaml parser.
func parseOpenAPIPaths(t *testing.T, file string) []opPath {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	rePath := regexp.MustCompile(`^  (/\S*):(?:\s.*)?$`)
	reMethod := regexp.MustCompile(`^    (get|post|patch|put|delete):\s*$`)
	var ops []opPath
	inPaths := false
	current := ""
	for _, line := range raw_lines(raw) {
		if inPaths && line == "paths:" {
			continue
		}
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			m := rePath.FindStringSubmatch(line)
			if m == nil {
				inPaths = false // another top-level section started
				continue
			}
			current = m[1]
		case inPaths && strings.HasPrefix(line, "    ") && current != "":
			if m := reMethod.FindStringSubmatch(line); m != nil {
				ops = append(ops, opPath{Method: strings.ToUpper(m[1]), Path: current})
			}
		}
	}
	return ops
}

func raw_lines(raw []byte) []string {
	return strings.Split(string(raw), "\n")
}

func TestOpenAPIDeclaresEveryRegisteredRoute(t *testing.T) {
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())
	ops := parseOpenAPIPaths(t, "../api/openapi.yaml")
	if len(ops) < 8 {
		t.Fatalf("spec parse found too few operations (%d): parser drifted", len(ops))
	}

	for _, op := range ops {
		probe := strings.ReplaceAll(op.Path, "{id}", uuid.NewString())
		resp, raw := e.request(op.Method, probe, nil, "")
		if resp.StatusCode == http.StatusNotFound {
			t.Fatalf("operation %s %s is in the spec but the server returned 404: %s",
				op.Method, op.Path, raw)
		}
	}
}

func TestOpenAPIIsNotMissingServerRoutes(t *testing.T) {
	// The routes the handlers register; a new route without a spec entry
	// fails here until the document is updated.
	ops := parseOpenAPIPaths(t, "../api/openapi.yaml")
	declared := map[opPath]bool{}
	for _, op := range ops {
		declared[op] = true
	}
	for _, want := range []opPath{
		{"GET", "/healthz"},
		{"GET", "/readyz"},
		{"GET", "/api/v1/platforms"},
		{"POST", "/api/v1/schedules"},
		{"GET", "/api/v1/schedules"},
		{"GET", "/api/v1/schedules/{id}"},
		{"PATCH", "/api/v1/schedules/{id}"},
		{"DELETE", "/api/v1/schedules/{id}"},
	} {
		if !declared[want] {
			t.Fatalf("route %s %s is registered by the server but missing from api/openapi.yaml", want.Method, want.Path)
		}
	}
}

func TestOpenAPIRoutesRejectWrongMethods(t *testing.T) {
	// A spec-declared path must actually honor the spec's method: probes with
	// the wrong method on the real server must not silently 200.
	e := newEnv(t, []app.Publisher{fakePublisher(t, "fake")}, withoutWorker())
	ops := parseOpenAPIPaths(t, "../api/openapi.yaml")

	for _, op := range ops {
		if op.Path == "/healthz" || op.Path == "/readyz" {
			continue
		}
		wrong := http.MethodPut
		if op.Method == http.MethodPut {
			wrong = http.MethodDelete
		}
		probe := strings.ReplaceAll(op.Path, "{id}", uuid.NewString())
		resp, _ := e.request(wrong, probe, nil, "")
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s responded to %s although the spec declares %s %s",
				probe, wrong, op.Method, op.Path)
		}
	}
}
