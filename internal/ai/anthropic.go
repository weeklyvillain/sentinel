package ai

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// DefaultAnthropicModel is the default model for the Anthropic provider.
const DefaultAnthropicModel = "claude-haiku-5-5"

// Anthropic drafts issues with Claude via the official Go SDK.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic builds a provider. An empty apiKey falls back to the SDK's
// default credential resolution (ANTHROPIC_API_KEY, profiles, ...).
func NewAnthropic(apiKey, model, baseURL string) *Anthropic {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if model == "" {
		model = DefaultAnthropicModel
	}
	return &Anthropic{client: anthropic.NewClient(opts...), model: model}
}

func (a *Anthropic) Name() string { return "anthropic:" + a.model }

// ErrRefused is returned when the model declines to answer.
var ErrRefused = errors.New("model refused the request")

func (a *Anthropic) Draft(ctx context.Context, in Input) (Draft, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 4096,
		System:    []anthropic.TextBlockParam{{Text: SystemPrompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(UserPrompt(in))),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: DraftSchema},
			Effort: anthropic.OutputConfigEffortLow,
		},
	})
	if err != nil {
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			switch apierr.StatusCode {
			case 401, 403:
				return Draft{}, fmt.Errorf("anthropic auth error (%d): %w", apierr.StatusCode, err)
			case 404:
				return Draft{}, fmt.Errorf("anthropic model %q not found: %w", a.model, err)
			case 429:
				return Draft{}, fmt.Errorf("anthropic rate limited: %w", err)
			default:
				return Draft{}, fmt.Errorf("anthropic API error (%d): %w", apierr.StatusCode, err)
			}
		}
		return Draft{}, fmt.Errorf("anthropic request failed: %w", err)
	}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return Draft{}, fmt.Errorf("%w: category=%q", ErrRefused, resp.StopDetails.Category)
	case anthropic.StopReasonMaxTokens:
		return Draft{}, errors.New("anthropic response truncated (max_tokens)")
	}
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			return parseDraft(tb.Text)
		}
	}
	return Draft{}, errors.New("anthropic response had no text block")
}
