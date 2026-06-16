package censor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go"
	openaiopt "github.com/openai/openai-go/option"
	"google.golang.org/genai"
)

// chatImage is a single image to send to a vision model, as raw bytes plus its
// MIME type (e.g. "image/png").
type chatImage struct {
	mimeType string
	data     []byte
}

func (im chatImage) base64() string {
	return base64.StdEncoding.EncodeToString(im.data)
}

// chatRequest is a provider-agnostic single-turn request. system carries the
// classifier instructions, text the user message/caption, and images any
// attachments (empty for text-only calls).
type chatRequest struct {
	model       string
	system      string
	text        string
	images      []chatImage
	maxTokens   int
	temperature float64
}

// chatProvider adapts one vendor's SDK to a single complete() call that returns
// the model's raw text reply. This is the seam that keeps the rest of the censor
// package independent of any particular vendor SDK. Implementations are stateless
// apart from credentials and are safe for concurrent use.
type chatProvider interface {
	name() string
	complete(ctx context.Context, req chatRequest) (string, error)
}

// newProvider constructs the adapter for the named provider. apiKey may be empty
// for keyless endpoints; endpoint overrides the SDK's default base URL.
func newProvider(name, apiKey, endpoint string) (chatProvider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai":
		return newOpenAIProvider(apiKey, endpoint), nil
	case "anthropic":
		return newAnthropicProvider(apiKey, endpoint), nil
	case "google", "gemini":
		return newGoogleProvider(apiKey, endpoint)
	default:
		return nil, fmt.Errorf("unknown provider %q (want openai|anthropic|google)", name)
	}
}

// --- OpenAI (and OpenAI-compatible endpoints) ---

type openAIProvider struct {
	client openai.Client
}

func newOpenAIProvider(apiKey, endpoint string) *openAIProvider {
	var opts []openaiopt.RequestOption
	if apiKey != "" {
		opts = append(opts, openaiopt.WithAPIKey(apiKey))
	}
	if endpoint != "" {
		opts = append(opts, openaiopt.WithBaseURL(endpoint))
	}
	return &openAIProvider{client: openai.NewClient(opts...)}
}

func (p *openAIProvider) name() string { return "openai" }

func (p *openAIProvider) complete(ctx context.Context, req chatRequest) (string, error) {
	var messages []openai.ChatCompletionMessageParamUnion
	if req.system != "" {
		messages = append(messages, openai.SystemMessage(req.system))
	}
	if len(req.images) == 0 {
		messages = append(messages, openai.UserMessage(req.text))
	} else {
		parts := []openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(req.text)}
		for _, im := range req.images {
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: fmt.Sprintf("data:%s;base64,%s", im.mimeType, im.base64()),
			}))
		}
		messages = append(messages, openai.UserMessage(parts))
	}

	resp, err := p.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:       openai.ChatModel(req.model),
		Messages:    messages,
		MaxTokens:   openai.Int(int64(req.maxTokens)),
		Temperature: openai.Float(req.temperature),
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// --- Anthropic ---

type anthropicProvider struct {
	client anthropic.Client
}

func newAnthropicProvider(apiKey, endpoint string) *anthropicProvider {
	var opts []anthropicopt.RequestOption
	if apiKey != "" {
		opts = append(opts, anthropicopt.WithAPIKey(apiKey))
	}
	if endpoint != "" {
		opts = append(opts, anthropicopt.WithBaseURL(endpoint))
	}
	return &anthropicProvider{client: anthropic.NewClient(opts...)}
}

func (p *anthropicProvider) name() string { return "anthropic" }

func (p *anthropicProvider) complete(ctx context.Context, req chatRequest) (string, error) {
	blocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(req.text)}
	for _, im := range req.images {
		blocks = append(blocks, anthropic.NewImageBlockBase64(im.mimeType, im.base64()))
	}

	params := anthropic.MessageNewParams{
		Model:       anthropic.Model(req.model),
		MaxTokens:   int64(req.maxTokens),
		Temperature: anthropic.Float(req.temperature),
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
	}
	if req.system != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.system}}
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// --- Google Gemini ---

type googleProvider struct {
	client *genai.Client
}

func newGoogleProvider(apiKey, endpoint string) (*googleProvider, error) {
	cfg := &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	}
	if endpoint != "" {
		cfg.HTTPOptions.BaseURL = endpoint
	}
	client, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &googleProvider{client: client}, nil
}

func (p *googleProvider) name() string { return "google" }

func (p *googleProvider) complete(ctx context.Context, req chatRequest) (string, error) {
	parts := []*genai.Part{genai.NewPartFromText(req.text)}
	for _, im := range req.images {
		parts = append(parts, genai.NewPartFromBytes(im.data, im.mimeType))
	}
	contents := []*genai.Content{{Parts: parts}}

	temperature := float32(req.temperature)
	config := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(req.maxTokens),
		Temperature:     &temperature,
	}
	if req.system != "" {
		config.SystemInstruction = &genai.Content{Parts: []*genai.Part{genai.NewPartFromText(req.system)}}
	}

	resp, err := p.client.Models.GenerateContent(ctx, req.model, contents, config)
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}
