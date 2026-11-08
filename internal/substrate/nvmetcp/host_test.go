package nvmetcp

import (
	"errors"
	"fmt"
	"testing"
)

// A command's error status reads as its type and code, and is told apart from a broken queue.
func TestStatusError(t *testing.T) {
	err := fmt.Errorf("connect: %w", StatusError(0x182))
	if got := err.Error(); got != "connect: status 0x182 (type 1, code 0x82)" {
		t.Fatal(got)
	}
	var st StatusError
	if !errors.As(err, &st) || errors.Is(err, ErrBroken) {
		t.Fatalf("%v", err)
	}
}
