package agent

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestProviderError_Is(t *testing.T) {
	err := &types.ProviderError{Provider: "test", Kind: types.ErrorKindTransient, Err: errors.New("timeout")}
	if !errors.Is(err, types.ErrProviderFailed) {
		t.Error("ProviderError should match ErrProviderFailed")
	}
}

func TestProviderError_Unwrap(t *testing.T) {
	inner := errors.New("connection refused")
	err := &types.ProviderError{Provider: "test", Err: inner}
	if !errors.Is(err, inner) {
		t.Error("should unwrap to inner error")
	}
}

func TestProviderError_ErrorString(t *testing.T) {
	err := &types.ProviderError{Provider: "ollama", Model: "llama3", Code: 500, Err: errors.New("server error")}
	s := err.Error()
	if s != "provider ollama (model llama3, status 500): server error" {
		t.Errorf("Error() = %q", s)
	}

	err2 := &types.ProviderError{Provider: "ollama", Model: "llama3", Err: errors.New("timeout")}
	s2 := err2.Error()
	if s2 != "provider ollama (model llama3): timeout" {
		t.Errorf("Error() = %q", s2)
	}
}

func TestFallbackError_Is(t *testing.T) {
	err := &types.FallbackError{Errors: []error{errors.New("a"), errors.New("b")}}
	if !errors.Is(err, types.ErrProviderFailed) {
		t.Error("FallbackError should match ErrProviderFailed")
	}
}

func TestFallbackError_UnwrapsToTheLastAttempt(t *testing.T) {
	first, last := errors.New("first"), errors.New("last")
	err := &types.FallbackError{Errors: []error{first, last}}
	if !errors.Is(err, last) || errors.Is(err, first) {
		t.Error("FallbackError should unwrap to its last attempt only")
	}
}

func TestRetryError_Unwrap(t *testing.T) {
	inner := &types.ProviderError{Provider: "test", Kind: types.ErrorKindTransient, Err: errors.New("timeout")}
	err := &types.RetryError{Attempts: 3, Last: inner}
	if !errors.Is(err, types.ErrProviderFailed) {
		t.Error("RetryError should unwrap through ProviderError to match ErrProviderFailed")
	}
}

func TestIsTransient(t *testing.T) {
	transient := &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("timeout")}
	permanent := &types.ProviderError{Kind: types.ErrorKindPermanent, Err: errors.New("unauthorized")}
	plain := errors.New("something")

	if !types.IsTransient(transient) {
		t.Error("expected transient")
	}
	if types.IsTransient(permanent) {
		t.Error("expected not transient")
	}
	if types.IsTransient(plain) {
		t.Error("expected not transient for plain error")
	}
}

func TestIsTransient_FallbackError(t *testing.T) {
	transient := &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("timeout")}
	fe := &types.FallbackError{Errors: []error{transient}}
	if !types.IsTransient(fe) {
		t.Error("FallbackError with transient last error should be transient")
	}

	permanent := &types.ProviderError{Kind: types.ErrorKindPermanent, Err: errors.New("auth")}
	fe2 := &types.FallbackError{Errors: []error{permanent}}
	if types.IsTransient(fe2) {
		t.Error("FallbackError with permanent last error should not be transient")
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		code int
		want types.ErrorKind
	}{
		{200, types.ErrorKindPermanent},
		{400, types.ErrorKindPermanent},
		{401, types.ErrorKindPermanent},
		{403, types.ErrorKindPermanent},
		{404, types.ErrorKindPermanent},
		{408, types.ErrorKindTransient},
		{429, types.ErrorKindTransient},
		{500, types.ErrorKindTransient},
		{502, types.ErrorKindTransient},
		{503, types.ErrorKindTransient},
	}
	for _, tt := range tests {
		got := types.ClassifyHTTPStatus(tt.code)
		if got != tt.want {
			t.Errorf("ClassifyHTTPStatus(%d) = %d, want %d", tt.code, got, tt.want)
		}
	}
}
