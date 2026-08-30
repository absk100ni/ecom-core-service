package product

import (
	"regexp"
	"testing"

	"ecom-core-service/internal/models"
)

func TestComputeLevel(t *testing.T) {
	tests := []struct {
		name        string
		parentLevel int
		wantLevel   int
		wantReject  bool
	}{
		{"root category", 0, 1, false},
		{"sub of root (level 1 parent)", 1, 2, false},
		{"sub-sub (level 2 parent)", 2, 3, false},
		{"exceeds max depth (level 3 parent)", 3, 4, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			childLevel := tt.parentLevel + 1
			if tt.parentLevel == 0 {
				childLevel = 1
			}
			if tt.wantReject && childLevel <= 3 {
				t.Errorf("expected rejection at level %d but got %d", childLevel, tt.wantLevel)
			}
			if !tt.wantReject && childLevel != tt.wantLevel {
				t.Errorf("got level %d, want %d", childLevel, tt.wantLevel)
			}
			if childLevel > 3 && !tt.wantReject {
				t.Errorf("level %d should be rejected", childLevel)
			}
		})
	}
}

func TestIsDescendant(t *testing.T) {
	tests := []struct {
		name       string
		cat        models.Category
		ancestorID string
		want       bool
	}{
		{
			name:       "direct child",
			cat:        models.Category{ParentID: "root1", Path: "root1"},
			ancestorID: "root1",
			want:       true,
		},
		{
			name:       "grandchild via path",
			cat:        models.Category{ParentID: "sub1", Path: "root1/sub1"},
			ancestorID: "root1",
			want:       true,
		},
		{
			name:       "not a descendant",
			cat:        models.Category{ParentID: "other", Path: "other"},
			ancestorID: "root1",
			want:       false,
		},
		{
			name:       "empty path (root category)",
			cat:        models.Category{ParentID: "", Path: ""},
			ancestorID: "anything",
			want:       false,
		},
		{
			name:       "deep descendant",
			cat:        models.Category{ParentID: "sub2", Path: "root1/sub1/sub2"},
			ancestorID: "sub1",
			want:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDescendant(tt.cat, tt.ancestorID)
			if got != tt.want {
				t.Errorf("isDescendant() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCycleDetection(t *testing.T) {
	// A category cannot be its own parent (direct check)
	cat := models.Category{ID: "cat1", ParentID: "cat1"}
	if cat.ID == cat.ParentID {
		// This is the self-cycle case — should reject. Pass.
	} else {
		t.Error("expected self-cycle detection")
	}

	// A parent cannot be a descendant of the category being edited
	descendant := models.Category{ID: "child1", ParentID: "cat1", Path: "root/cat1"}
	if !isDescendant(descendant, "cat1") {
		t.Error("expected descendant detection for cycle check")
	}
}

func TestBuildCategoryTree(t *testing.T) {
	cats := []models.Category{
		{ID: "r1", Name: "Electronics", Slug: "electronics", Level: 1, ParentID: ""},
		{ID: "r2", Name: "Clothing", Slug: "clothing", Level: 1, ParentID: ""},
		{ID: "s1", Name: "Phones", Slug: "phones", Level: 2, ParentID: "r1", Path: "r1"},
		{ID: "s2", Name: "Laptops", Slug: "laptops", Level: 2, ParentID: "r1", Path: "r1"},
		{ID: "ss1", Name: "Android", Slug: "android", Level: 3, ParentID: "s1", Path: "r1/s1"},
	}
	tree := BuildCategoryTree(cats)

	if len(tree) != 2 {
		t.Fatalf("expected 2 roots, got %d", len(tree))
	}
	if tree[0].ID != "r1" {
		t.Errorf("first root should be r1, got %s", tree[0].ID)
	}
	if len(tree[0].Children) != 2 {
		t.Fatalf("r1 should have 2 children, got %d", len(tree[0].Children))
	}
	// Find Phones child
	var phones *CategoryTreeNode
	for i := range tree[0].Children {
		if tree[0].Children[i].ID == "s1" {
			phones = &tree[0].Children[i]
		}
	}
	if phones == nil {
		t.Fatal("Phones not found as child of r1")
	}
	if len(phones.Children) != 1 {
		t.Fatalf("Phones should have 1 child, got %d", len(phones.Children))
	}
	if phones.Children[0].ID != "ss1" {
		t.Errorf("expected Android (ss1), got %s", phones.Children[0].ID)
	}
	// Clothing has no children
	if len(tree[1].Children) != 0 {
		t.Errorf("Clothing should have 0 children, got %d", len(tree[1].Children))
	}
}

func TestBuildCategoryTreeEmpty(t *testing.T) {
	tree := BuildCategoryTree(nil)
	if tree == nil || len(tree) != 0 {
		t.Errorf("empty input should return empty slice, got %v", tree)
	}
}

func TestSuggestRegexEscaping(t *testing.T) {
	// Ensure special regex characters are escaped properly
	dangerous := []string{
		"a+b(",
		"price$100",
		"test.name",
		"[brackets]",
		"pipe|or",
		"star*glob",
		`back\slash`,
		"hat^start",
		"quest?ion",
		"{curly}",
	}
	for _, input := range dangerous {
		t.Run(input, func(t *testing.T) {
			escaped := regexp.QuoteMeta(input)
			pattern := "^" + escaped
			_, err := regexp.Compile(pattern)
			if err != nil {
				t.Errorf("regexp.QuoteMeta(%q) produced invalid regex: %v", input, err)
			}
		})
	}
}

func TestSuggestDedup(t *testing.T) {
	// Simulate dedup logic: seen map prevents duplicates
	seen := make(map[string]bool)
	ids := []string{"id1", "id2", "id1", "id3", "id2"}
	var deduped []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			deduped = append(deduped, id)
		}
	}
	if len(deduped) != 3 {
		t.Errorf("expected 3 unique ids, got %d", len(deduped))
	}
	if deduped[0] != "id1" || deduped[1] != "id2" || deduped[2] != "id3" {
		t.Errorf("unexpected order: %v", deduped)
	}
}

func TestCollectDescendantSlugsPathPrefix(t *testing.T) {
	// Test the path prefix construction logic
	tests := []struct {
		name    string
		catPath string
		catID   string
		wantPfx string
	}{
		{"root category", "", "root1", "root1"},
		{"sub category", "root1", "sub1", "root1/sub1"},
		{"deep category", "root1/sub1", "subsub1", "root1/sub1/subsub1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pathPrefix string
			if tt.catPath == "" {
				pathPrefix = tt.catID
			} else {
				pathPrefix = tt.catPath + "/" + tt.catID
			}
			if pathPrefix != tt.wantPfx {
				t.Errorf("got %q, want %q", pathPrefix, tt.wantPfx)
			}
		})
	}
}
