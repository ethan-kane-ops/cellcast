package boundaries_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// describing are the calls that put something on a span: an attribute, a
// status, an event or a link. Starting and ending a span describes nothing.
var describing = []string{"SetAttributes", "SetStatus", "AddEvent", "RecordError", "AddLink", "WithAttributes", "WithLinks"}

// TestOnlyTracingFilesDescribeSpans keeps what a span carries in the files
// written to decide it.
//
// A span attribute is a token-material surface exactly like a log field
// (docs/threat-model.md T-05), and the hub's come from the audit record's
// classification. That holds only while nothing else sets one: a helpful
// span.SetAttributes(attribute.String("authorization", ...)) three packages
// away would pass every other test there is. Starting and ending spans is
// allowed anywhere; describing one only in these files.
func TestOnlyTracingFilesDescribeSpans(t *testing.T) {
	root := repoRoot(t)
	allowed := []string{"internal/hub/tracing.go", "internal/agent/tracing.go", "internal/telemetry/telemetry.go"}
	for _, f := range allowed {
		// A renamed tracing file would otherwise exempt nothing and still pass.
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("the allowlist names %s, which does not exist", f)
		}
	}

	fset := token.NewFileSet()
	var scanned int
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			if slices.Contains(allowed, filepath.ToSlash(rel)) {
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(describing, sel.Sel.Name) {
					t.Errorf("%s: %s puts something on a span outside the tracing files; "+
						"move it into one of %v, where what a span carries is decided in one place",
						fset.Position(call.Pos()), sel.Sel.Name, allowed)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if scanned == 0 {
		t.Fatal("no Go files were scanned, so this test checked nothing")
	}
}

// TestTheClientDoesNotCarryTelemetry keeps OTLP out of the binary a pipeline
// downloads on every run, for the same reason the typed clientset is kept out:
// it has no collector to report to, and the exporters bring gRPC and a
// protobuf runtime with them.
func TestTheClientDoesNotCarryTelemetry(t *testing.T) {
	for _, pkg := range deps(t, mod+"/cmd/cellcast") {
		if strings.HasPrefix(pkg, "go.opentelemetry.io/") || strings.HasPrefix(pkg, "google.golang.org/grpc") ||
			pkg == mod+"/internal/telemetry" {
			t.Errorf("the client links %s", pkg)
		}
	}
}

// TestServerBuildsLeaveOutGRPCTrace holds the build tag that keeps the hub and
// the agent small.
//
// The OTLP exporters bring gRPC, gRPC imports golang.org/x/net/trace, and that
// imports html/template. With html/template in a binary the linker can no
// longer discard unused methods anywhere in it, which cost the hub 20 MB and
// the agent 28 MB. grpcnotrace drops the import. A build that forgets the tag
// still works and is merely enormous, which is why a test has to notice.
func TestServerBuildsLeaveOutGRPCTrace(t *testing.T) {
	root := repoRoot(t)

	justfile, err := os.ReadFile(filepath.Join(root, "justfile"))
	if err != nil {
		t.Fatalf("reading the justfile: %v", err)
	}
	if !strings.Contains(string(justfile), `go_tags := "grpcnotrace"`) {
		t.Error(`the justfile does not set go_tags := "grpcnotrace"`)
	}
	var builds int
	for _, line := range strings.Split(string(justfile), "\n") {
		if !strings.Contains(line, "go build -trimpath") {
			continue
		}
		builds++
		if !strings.Contains(line, "-tags '{{go_tags}}'") {
			t.Errorf("a justfile build does not pass the tags:\n%s", strings.TrimSpace(line))
		}
	}
	if builds == 0 {
		t.Error("found no go build in the justfile, so the recipes were not checked")
	}

	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the Dockerfile: %v", err)
	}
	if !regexp.MustCompile(`go build[^\n]*-tags grpcnotrace`).Match(dockerfile) {
		t.Error("the Dockerfile, which builds the hub and agent images, does not pass -tags grpcnotrace")
	}

	for _, bin := range []string{"cellcast-hub", "cellcast-agent"} {
		if slices.Contains(listDeps(t, "-tags=grpcnotrace", mod+"/cmd/"+bin), "html/template") {
			t.Errorf("%s links html/template even with grpcnotrace; something else imports it now", bin)
		}
		// And the tag is still what makes the difference. When it stops being,
		// gRPC has dropped the import by itself and the tag can go.
		if !slices.Contains(listDeps(t, "", mod+"/cmd/"+bin), "html/template") {
			t.Errorf("%s no longer links html/template without grpcnotrace; the tag does nothing now and can be removed", bin)
		}
	}
}

// listDeps is `go list -deps` with optional build tags.
func listDeps(t *testing.T, tags, pkg string) []string {
	t.Helper()
	args := []string{"list", "-deps"}
	if tags != "" {
		args = append(args, tags)
	}
	out, err := exec.Command("go", append(args, pkg)...).Output()
	if err != nil {
		t.Fatalf("go list -deps %s %s: %v", tags, pkg, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
