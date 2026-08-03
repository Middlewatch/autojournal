package autojournal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusReportsNotBuiltForRootThatDoesNotExist(t *testing.T) {
	base := t.TempDir()
	report := StatusOf(filepath.Join(base, "absent"), filepath.Join(base, "index.sqlite"))
	if report.RootOK {
		t.Error("root_ok for a missing root")
	}
	if report.Healthy() {
		t.Error("healthy for a missing root")
	}
	if report.Freshness != IndexNotBuilt {
		t.Errorf("freshness = %q", report.Freshness)
	}
}

func TestSyncRefusesJournalRootUnderSharedDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "shared", "journals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := Sync(filepath.Join(base, "shared", "journals"), filepath.Join(base, "index.sqlite"))
	if !errors.Is(err, ErrSharedDirectory) {
		t.Errorf("err = %v, want ErrSharedDirectory", err)
	}
}

// The accounting rules the module exists for: sync stamps identity and
// exclusions, status reads them back without contradiction, and the
// redelivery check classifies from the stored file's own frontmatter.
func TestSyncStatusRoundTripAndRedeliveryClassification(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "journals")
	indexPath := filepath.Join(base, "state", "index.sqlite")

	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatalf("OpenJournalRoot: %v", err)
	}
	defer root.Close()
	payload := mustValidate(t, testPayloadJSON)
	pub := mustPublish(t, root, payload)
	second := payload
	second.TurnID = "turn-0099"
	mustPublish(t, root, second)

	report, err := Sync(rootPath, indexPath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Indexed != 2 {
		t.Errorf("indexed = %d", report.Indexed)
	}

	st := StatusOf(rootPath, indexPath)
	if !st.RootOK || st.Episodes != 2 || st.Indexed != 2 {
		t.Errorf("status = %+v", st)
	}
	if !st.Healthy() || st.Freshness != IndexFresh {
		t.Errorf("freshness = %q", st.Freshness)
	}

	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	// The same payload redelivered anywhere in the corpus is a duplicate.
	dup := CheckRedelivery(root, idx, &payload)
	if dup == nil || dup.Outcome != CaptureDuplicate {
		t.Fatalf("redelivery = %+v, want duplicate", dup)
	}
	if dup.RelPath != pub.RelPath {
		t.Errorf("rel_path = %q, want %q", dup.RelPath, pub.RelPath)
	}

	// The same identity with different content is a conflict.
	altered := payload
	altered.UserContent = "rewritten after the fact"
	conflict := CheckRedelivery(root, idx, &altered)
	if conflict == nil || conflict.Outcome != CaptureConflict {
		t.Fatalf("redelivery = %+v, want conflict", conflict)
	}

	// An unknown identity is not a redelivery: the caller publishes.
	unknown := payload
	unknown.TurnID = "turn-9999"
	if red := CheckRedelivery(root, idx, &unknown); red != nil {
		t.Errorf("redelivery = %+v, want nil", red)
	}
}
