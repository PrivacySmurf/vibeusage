package provider

import "context"

// DiagnosticStatus classifies a single auth-diagnostic observation.
type DiagnosticStatus string

const (
	// DiagOK means the observed state is healthy.
	DiagOK DiagnosticStatus = "ok"
	// DiagWarn means the state is suspicious but not necessarily broken.
	DiagWarn DiagnosticStatus = "warn"
	// DiagFail means the state is broken or unreachable.
	DiagFail DiagnosticStatus = "fail"
	// DiagInfo is a neutral observation with no health judgement.
	DiagInfo DiagnosticStatus = "info"
)

// Diagnostic is one read-only observation about a provider's credential or
// session state. Details carry names, ages, sources, and errors only — never
// secret values such as cookie contents or tokens.
type Diagnostic struct {
	Name   string
	Status DiagnosticStatus
	Detail string
}

// AuthDiagnoser is implemented by providers that can inspect their own
// credential and session state without mutating anything. It backs
// `vibeusage auth <provider> --diagnose`.
type AuthDiagnoser interface {
	DiagnoseAuth(ctx context.Context) []Diagnostic
}
