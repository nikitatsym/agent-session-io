package presence

import (
	"context"

	sessionio "github.com/nikitatsym/agent-session-io"
)

type unavailableProvider struct {
	harness sessionio.Harness
	detail  string
}

// NewUnavailableProvider reports an unimplemented capability without inspecting processes.
func NewUnavailableProvider(harness sessionio.Harness, detail string) Provider {
	return unavailableProvider{harness: harness, detail: detail}
}
func (provider unavailableProvider) Harness() sessionio.Harness { return provider.harness }
func (provider unavailableProvider) Inspect(ctx context.Context, _ []sessionio.SessionRef) (ProviderResult, error) {
	if err := ctx.Err(); err != nil {
		return ProviderResult{}, err
	}
	reason := sessionio.PresenceReasonProviderUnavailable
	status := providerStatus(provider.harness, "1", sessionio.PresenceSupportUnavailable, &reason, provider.detail, provider.detail)
	status.Capabilities[1].Support = sessionio.PresenceSupportUnavailable
	status.Capabilities[1].Reason = &reason
	return ProviderResult{Status: status}, nil
}
