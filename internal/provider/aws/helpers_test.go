package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/smithy-go"
)

// The helpers here all answer the same question in different words: "is this
// error saying the thing is not there?" Each one is on a path where getting it
// wrong turns a harmless no-op into a spurious failure, or a real failure into a
// silent success, so each is pinned.

// TestAbsentMessageRecognisesEachWording: services answer a missing resource in
// several shapes and this recognises the three the SSM client produces. A
// "parameter not found" must read as absent, so deleting a parameter that is
// already gone is a success rather than an error.
func TestAbsentMessageRecognisesEachWording(t *testing.T) {
	for _, msg := range []string{
		"Parameter not found",
		"parameter not found: /dawnbx/x",
		"The parameter does not exist",
		"Cluster not found",
		"NOT FOUND", // case must not matter
	} {
		if !absentMessage(errors.New(msg)) {
			t.Errorf("%q was not recognised as absent", msg)
		}
	}
	// A real failure must not be mistaken for absence, or a genuine error would
	// be swallowed into a success.
	for _, msg := range []string{
		"AccessDeniedException: not authorised to perform ssm:DeleteParameter",
		"ThrottlingException: rate exceeded",
		"InvalidParameterValue",
	} {
		if absentMessage(errors.New(msg)) {
			t.Errorf("%q was mistaken for absence", msg)
		}
	}
	// A nil error is not absence: absence is a claim about a failed call.
	if absentMessage(nil) {
		t.Error("nil was treated as absence")
	}
}

// TestInstanceGoneReadsTheCodeNotTheText: EC2's answer is a code, and a launch
// refused for quota must not be read as "gone", because that would make a
// retriable failure look like success.
func TestInstanceGoneReadsTheCodeNotTheText(t *testing.T) {
	for _, code := range []string{"InvalidInstanceID.NotFound", "InvalidInstanceID.Malformed"} {
		if !instanceGone(coded(code)) {
			t.Errorf("%s was not recognised as gone", code)
		}
	}
	for _, code := range []string{
		"InsufficientInstanceCapacity",
		"RequestLimitExceeded",
		"UnauthorizedOperation",
		"",
	} {
		if instanceGone(coded(code)) {
			t.Errorf("%s was mistaken for a gone instance", code)
		}
	}
	if instanceGone(nil) {
		t.Error("nil was treated as a gone instance")
	}
	// An error with no code at all is not "gone" either.
	if instanceGone(errors.New("InvalidInstanceID.NotFound happened in prose")) {
		t.Error("prose was read as a code")
	}
}

// TestSleepHonoursACancelledContext: the settle loop must give up when the
// context is done, rather than sleeping out a five-minute teardown for a server
// that is shutting down.
func TestSleepHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := sleep(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled sleep returned %v", err)
	}
	// It must have returned promptly, not after the minute it was asked to wait.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the cancelled sleep waited %s", elapsed)
	}
}

func TestSleepReturnsWhenTheTimerFires(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("an ordinary sleep: %v", err)
	}
}

// Terminating an already-gone instance, and reporting a real refusal, are covered
// in compute_test.go through the shared EC2 harness, which builds a real adapter
// against both reply shapes. Repeating it here would test the harness, not the
// adapter.

// coded builds an error carrying an API code, the way the AWS clients do. A
// GenericAPIError is the closest stand-in: the adapter asks for ErrorCode()
// through an interface, so any error that supplies one is the same shape.
func coded(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code}
}
