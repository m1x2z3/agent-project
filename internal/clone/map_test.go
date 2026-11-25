package clone

import "testing"

func TestMapCopiesContainerWithoutSharingEntries(t *testing.T) {
	source := map[string]string{"task_analysis": "v1"}
	copied := Map(source)
	copied["task_analysis"] = "v2"
	copied["final_answer"] = "v1"

	if source["task_analysis"] != "v1" {
		t.Fatalf("source value = %q, want v1", source["task_analysis"])
	}
	if _, exists := source["final_answer"]; exists {
		t.Fatalf("source gained copied key: %+v", source)
	}
}

func TestMapPreservesNilMap(t *testing.T) {
	var source map[string]string
	if copied := Map(source); copied != nil {
		t.Fatalf("copied nil map = %#v, want nil", copied)
	}
}
