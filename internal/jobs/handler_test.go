package jobs

import (
	"errors"
	"testing"
)

func TestPermanentWrapsErrorAndPreservesNil(t *testing.T) {
	want := errors.New("invalid input")
	err := Permanent(want)
	var permanent *PermanentError
	if !errors.As(err, &permanent) || !errors.Is(err, want) {
		t.Fatalf("Permanent(error) = %v, want PermanentError wrapping source", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must return nil")
	}
}
