package runner

import (
	"errors"
	"testing"
)

func TestRunnerErrorsAreStableSentinels(t *testing.T) {
	errs := []error{
		ErrExecutorNotConfigured,
		ErrInvalidToolCallRequest,
		ErrInvalidToolDecision,
		ErrToolVersionRequired,
		ErrArgumentsEncodingFailed,
	}

	for _, err := range errs {
		if err == nil {
			t.Fatal("runner error must not be nil")
		}
		if !errors.Is(err, err) {
			t.Fatalf("errors.Is(%v, %v) = false", err, err)
		}
	}
}
