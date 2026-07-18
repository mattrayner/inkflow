package frontmatter

import (
	"strings"
	"testing"
)

func TestUpdateTagsPreservesBody(t *testing.T) {
	got := UpdateTags("---\ntags:\n  - old\n---\n\n# Title\nBody\n", []string{"new", "tag"})
	if !strings.Contains(got, "  - new\n") || !strings.Contains(got, "  - tag\n") {
		t.Fatalf("tags not updated:\n%s", got)
	}
	if !strings.Contains(got, "# Title\nBody\n") {
		t.Fatalf("body changed:\n%s", got)
	}
}

func TestUpdateTagsAddsFrontmatter(t *testing.T) {
	got := UpdateTags("# Title\nBody\n", []string{"one", "two", "one"})
	if !strings.HasPrefix(got, "---\n") {
		t.Fatalf("missing frontmatter:\n%s", got)
	}
	if !strings.Contains(got, "  - one\n") || !strings.Contains(got, "  - two\n") {
		t.Fatalf("tags missing:\n%s", got)
	}
	if !strings.HasSuffix(got, "# Title\nBody\n") {
		t.Fatalf("body changed:\n%s", got)
	}
}

func TestUpdateTagsMergesExistingTagsInStableOrder(t *testing.T) {
	got := UpdateTags("---\ntags:\n  - manual\n  - project\n---\nBody\n", []string{"project", "meeting", "manual"})
	want := "tags:\n    - manual\n    - project\n    - meeting"
	if !strings.Contains(got, want) {
		t.Fatalf("merged tags missing from %q", got)
	}
}

func TestUpdateTagsReplaceStrategy(t *testing.T) {
	got := UpdateTagsWithStrategy("---\ntags:\n  - manual\n---\nBody\n", []string{"project", "project"}, "replace")
	if strings.Contains(got, "manual") || !strings.Contains(got, "- project") {
		t.Fatalf("replace tags = %q", got)
	}
}
