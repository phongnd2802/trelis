package file

import (
	"testing"

	"github.com/google/uuid"
)

func TestSourceKeyAndParseKeyRoundTrip(t *testing.T) {
	documentID, uploadID := uuid.New(), uuid.New()
	key := SourceKey(documentID, uploadID)

	if want := DocumentPrefix(documentID) + uploadID.String() + "/source"; key != want {
		t.Errorf("SourceKey = %q, want %q", key, want)
	}
	gotDocument, gotUpload, err := ParseKey(key)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", key, err)
	}
	if gotDocument != documentID || gotUpload != uploadID {
		t.Errorf("ParseKey = %s/%s, want %s/%s", gotDocument, gotUpload, documentID, uploadID)
	}
}

func TestParseKeyRejectsKeysOutsideTheLayout(t *testing.T) {
	for _, key := range []string{
		"figures/1.png",
		"documents/" + uuid.NewString() + "/source",
		"documents/not-a-document/" + uuid.NewString() + "/source",
		"documents/" + uuid.NewString() + "/not-an-upload/source",
	} {
		if _, _, err := ParseKey(key); err == nil {
			t.Errorf("ParseKey(%q) succeeded, want an error", key)
		}
	}
}
